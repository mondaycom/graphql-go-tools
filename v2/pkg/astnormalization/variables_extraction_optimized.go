package astnormalization

// This file holds the linearized variable-extraction helpers gated behind
// mondaytweaks.OptimizeVariablesExtraction (name generation + dedup) and
// mondaytweaks.BatchExtractedVariablesJSON (the in-place Input.Variables append). Together
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

// appendExtractedVariableRaw registers a newly extracted variable in Input.Variables without
// re-serialising the whole buffer, replacing the per-variable sjson.SetRawBytes write (which
// copies O(N) bytes per call, O(N^2) over the batch). It appends "name":value in place into a
// per-document owned buffer, so N extractions cost O(N) amortised.
//
// The buffer is copied on first use (never reused across documents) so we neither mutate the
// caller's shared Input.Variables backing array nor corrupt a previous operation's variables
// still referenced through its buffer. Input.Variables is repointed after every append and
// remains a valid JSON object throughout, so uploads.FindUploads — which reparses it on each
// argument — observes exactly the bytes it does on the sjson path.
func (v *variablesExtractionVisitor) appendExtractedVariableRaw(name, value []byte) {
	if !v.varsBufOwned {
		v.varsBuf = append([]byte(nil), v.operation.Input.Variables...)
		v.varsBufOwned = true
	}
	v.varsBuf = appendJSONObjectMember(v.varsBuf, name, value)
	v.operation.Input.Variables = v.varsBuf
}

// appendJSONObjectMember appends a raw "name":value member as the last entry of the JSON
// object in dst, keeping dst a valid object. dst may be empty, "null", "{}", or a populated
// object. Because extracted variable names are freshly generated and never already present,
// this matches where sjson.SetRawBytes places a not-yet-existing top-level key, so the output
// is byte-identical to the per-variable sjson path (asserted by the differential test). dst is
// mutated in place and may be reallocated on growth, exactly like append.
func appendJSONObjectMember(dst, name, value []byte) []byte {
	end := len(dst)
	for end > 0 && isJSONWhitespace(dst[end-1]) {
		end--
	}
	// Empty, "null", or any non-object payload: emit a fresh single-member object.
	if end == 0 || dst[end-1] != '}' {
		out := dst[:0]
		out = append(out, '{', '"')
		out = append(out, name...)
		out = append(out, '"', ':')
		out = append(out, value...)
		out = append(out, '}')
		return out
	}
	// dst[end-1] is the closing brace. Decide whether the object already has members by
	// looking at the last non-whitespace byte before it: '{' means empty.
	brace := end - 1
	j := brace
	for j > 0 && isJSONWhitespace(dst[j-1]) {
		j--
	}
	hasMembers := j > 0 && dst[j-1] != '{'

	out := dst[:brace]
	if hasMembers {
		out = append(out, ',')
	}
	out = append(out, '"')
	out = append(out, name...)
	out = append(out, '"', ':')
	out = append(out, value...)
	out = append(out, '}')
	return out
}

func isJSONWhitespace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}
