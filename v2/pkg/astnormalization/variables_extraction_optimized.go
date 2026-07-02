package astnormalization

// This file holds the linearized variable-extraction helpers gated behind
// mondaytweaks.OptimizeVariablesExtraction. They replace three super-linear hotspots on
// the aliased-batch mutation shape (see mondaytweaks.OptimizeVariablesExtraction):
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
