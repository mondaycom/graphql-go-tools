package astnormalization

import (
	"github.com/buger/jsonparser"
	"github.com/tidwall/sjson"

	"github.com/wundergraph/graphql-go-tools/v2/pkg/ast"
	"github.com/wundergraph/graphql-go-tools/v2/pkg/internal/unsafebytes"
)

// This file holds the linearized variable-extraction helpers gated behind
// mondaytweaks.OptimizeVariablesExtraction (name generation + dedup) and
// mondaytweaks.BatchExtractedVariablesJSON (the deferred Input.Variables build). Together
// they replace three super-linear hotspots on the aliased-batch mutation shape:
//
//   - nextGeneratedVariableName replaces Document.GenerateUnusedVariableDefinitionName,
//     which restarts its search from "a" and linearly scans the operation's variable
//     definitions for every candidate (O(N^3) over the batch). The optimized generator
//     walks the identical name sequence (a..z, aa..zz, aaa..zzz, ...) with a monotonic
//     cursor, skipping only pre-existing user variable names. Because every generated name
//     is added to the operation exactly once and the cursor never revisits an index, the
//     emitted sequence is byte-identical to the upstream generator.
//
//   - typeValueDedupKey + the dedupIndex map replace variableExists, which walked the whole
//     growing Input.Variables JSON and linearly scanned the extracted-variable list for
//     every argument (O(N^2)). Deduplication keys on (canonical type, raw JSON value); the
//     canonical type is rendered with PrintTypeBytes, which produces the same string for any
//     two types TypesAreEqualDeep considers equal, so the dedup decision is identical.
//
// The optimized path only runs for single-operation documents; see EnterDocument.

// optimizedVariableNameAlphabet must stay identical to ast.alphabet, which drives
// Document.GenerateUnusedVariableDefinitionName, so both name generators agree.
const optimizedVariableNameAlphabet = "abcdefghijklmnopqrstuvwxyz"

// buildPreExistingNames records the user-defined variable names present on the operation
// before extraction begins. Generated names must avoid these (the upstream generator skips
// them via its existence check); extracted names never need to be tracked here because the
// monotonic cursor never re-emits an index.
func (v *variablesExtractionVisitor) buildPreExistingNames(opRef int) {
	for _, r := range v.operation.OperationDefinitions[opRef].VariableDefinitions.Refs {
		name := v.operation.VariableValueNameBytes(v.operation.VariableDefinitions[r].VariableValue.Ref)
		v.preExistingNames[string(name)] = struct{}{}
	}
}

// nextGeneratedVariableName returns the next name in the sequence a..z, aa..zz, aaa..zzz,
// ... (each length uses a single repeated letter, exactly like the upstream generator),
// skipping any name that collides with a pre-existing user variable.
func (v *variablesExtractionVisitor) nextGeneratedVariableName() []byte {
	for {
		length := v.nameCursor/len(optimizedVariableNameAlphabet) + 1
		ch := optimizedVariableNameAlphabet[v.nameCursor%len(optimizedVariableNameAlphabet)]
		v.nameCursor++

		name := make([]byte, length)
		for i := range name {
			name[i] = ch
		}
		if _, taken := v.preExistingNames[string(name)]; !taken {
			return name
		}
	}
}

// typeValueDedupKey builds the deduplication key for an inline argument value. The key is
// the canonical rendering of the argument's input type, a NUL separator, and the raw JSON
// value bytes. PrintTypeBytes renders NonNull/List/Named exactly as TypesAreEqualDeep
// compares them, so deeply-equal types share a key and distinct types never collide.
func (v *variablesExtractionVisitor) typeValueDedupKey(inputValueDefinition int, valueBytes []byte) string {
	typeRef := v.definition.InputValueDefinitions[inputValueDefinition].Type

	typeKey, err := v.definition.PrintTypeBytes(typeRef, v.typeKeyScratch[:0])
	if err != nil {
		// PrintTypeBytes only fails on a malformed type ref. Fall back to a value-only key:
		// it may over-merge across types in this pathological case, but the upstream
		// validator rejects such operations before the result is used.
		return string(valueBytes)
	}
	v.typeKeyScratch = typeKey

	key := make([]byte, 0, len(typeKey)+1+len(valueBytes))
	key = append(key, typeKey...)
	key = append(key, 0)
	key = append(key, valueBytes...)
	return string(key)
}

// LeaveDocument flushes the variables buffered on the batched path into Input.Variables in a
// single build. On the default (sjson) path nothing is buffered — each variable is written in
// EnterArgument — so this is a no-op.
func (v *variablesExtractionVisitor) LeaveDocument(_, _ *ast.Document) {
	if !v.batchVarsJSON || len(v.pendingVarNames) == 0 {
		return
	}
	v.operation.Input.Variables = flushBatchedExtractedVariables(v.operation.Input.Variables, v.pendingVarNames, v.pendingVarValues)
}

// flushBatchedExtractedVariables builds the final Input.Variables object from the original
// buffer (the client-supplied variables, left untouched during extraction) and the extracted
// (name,value) pairs captured in first-occurrence order. It reproduces the per-variable
// sjson.SetRawBytes path byte-for-byte.
//
// sjson has set-or-replace semantics: a not-yet-present top-level key is inserted at the FRONT
// of the object, while an already-present key is overwritten in place (position and key
// formatting preserved). So after N sequential inserts the newly generated variables appear in
// reverse creation order ahead of any pre-existing client variables, and any generated name
// that happens to collide with a pre-seeded client variable overwrites it where it sits.
//
// The fast path is the aliased-batch target: the client sent no variables (orig is empty), so
// nothing can collide, every pair is new, and the whole object is built in a single
// O(total bytes) pass instead of sjson's O(N^2). When orig already has members we detect the
// (rare) colliding names and replay just those through sjson to reproduce its in-place
// overwrite, then front-prepend the genuinely new names in reverse creation order.
func flushBatchedExtractedVariables(orig []byte, names, values [][]byte) []byte {
	newNames, newValues := names, values

	if _, hasMembers := jsonObjectInner(orig); hasMembers {
		existing := existingTopLevelKeys(orig)
		if len(existing) > 0 {
			newNames, newValues = names[:0:0], values[:0:0]
			for i := range names {
				if _, collides := existing[string(names[i])]; collides {
					if buf, err := sjson.SetRawBytes(orig, unsafebytes.BytesToString(names[i]), values[i]); err == nil {
						orig = buf
					}
					continue
				}
				newNames = append(newNames, names[i])
				newValues = append(newValues, values[i])
			}
		}
	}

	if len(newNames) == 0 {
		return orig
	}

	inner, hasMembers := jsonObjectInner(orig)

	size := 2 + len(inner) // outer braces + preserved original members
	for i := range newNames {
		size += len(newNames[i]) + len(newValues[i]) + 4 // two quotes, colon, comma
	}

	out := make([]byte, 0, size)
	out = append(out, '{')
	for i := len(newNames) - 1; i >= 0; i-- {
		if i != len(newNames)-1 {
			out = append(out, ',')
		}
		out = append(out, '"')
		out = append(out, newNames[i]...)
		out = append(out, '"', ':')
		out = append(out, newValues[i]...)
	}
	if hasMembers {
		out = append(out, ',')
		out = append(out, inner...)
	}
	out = append(out, '}')
	return out
}

// existingTopLevelKeys returns the set of top-level object keys already present in src, used to
// detect generated names that collide with pre-seeded client variables. It is only built when
// src actually has members, so the aliased-batch fast path (empty client variables) never
// allocates it.
func existingTopLevelKeys(src []byte) map[string]struct{} {
	keys := make(map[string]struct{})
	_ = jsonparser.ObjectEach(src, func(key, _ []byte, _ jsonparser.ValueType, _ int) error {
		keys[string(key)] = struct{}{}
		return nil
	})
	return keys
}

// jsonObjectInner returns the content between the outer braces of the JSON object in src and
// whether that object has any members. An empty buffer, "null", or an empty object (any
// whitespace-only "{}") reports no members, matching sjson treating a missing/empty document
// as an empty object when it sets the first key.
func jsonObjectInner(src []byte) (inner []byte, hasMembers bool) {
	start := 0
	for start < len(src) && isJSONWhitespace(src[start]) {
		start++
	}
	end := len(src)
	for end > start && isJSONWhitespace(src[end-1]) {
		end--
	}
	if end-start < 2 || src[start] != '{' || src[end-1] != '}' {
		return nil, false
	}
	inner = src[start+1 : end-1]
	for i := range inner {
		if !isJSONWhitespace(inner[i]) {
			return inner, true
		}
	}
	return nil, false
}

func isJSONWhitespace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}
