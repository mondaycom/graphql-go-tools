package astnormalization

import (
	"bytes"
	"fmt"

	"github.com/buger/jsonparser"
	"github.com/tidwall/sjson"

	"github.com/wundergraph/graphql-go-tools/v2/pkg/ast"
	"github.com/wundergraph/graphql-go-tools/v2/pkg/astimport"
	"github.com/wundergraph/graphql-go-tools/v2/pkg/astnormalization/uploads"
	"github.com/wundergraph/graphql-go-tools/v2/pkg/astvisitor"
	"github.com/wundergraph/graphql-go-tools/v2/pkg/internal/unsafebytes"
	"github.com/wundergraph/graphql-go-tools/v2/pkg/mondaytweaks"
)

func extractVariables(walker *astvisitor.Walker) *variablesExtractionVisitor {
	visitor := &variablesExtractionVisitor{
		Walker:       walker,
		uploadFinder: uploads.NewUploadFinder(),
	}
	walker.RegisterEnterDocumentVisitor(visitor)
	walker.RegisterEnterArgumentVisitor(visitor)
	return visitor
}

type variablesExtractionVisitor struct {
	*astvisitor.Walker

	operation, definition     *ast.Document
	importer                  astimport.Importer
	skip                      bool
	extractedVariables        [][]byte
	extractedVariableTypeRefs []int
	uploadFinder              *uploads.UploadFinder
	uploadsPath               []uploads.UploadPathMapping

	// optimized-path state (mondaytweaks.OptimizeVariablesExtraction, single-operation
	// documents only). See variables_extraction_optimized.go.
	optimize              bool
	preExistingNames      map[string]struct{} // user-defined variable names present before extraction
	preExistingNamesBuilt bool
	nameCursor            int               // monotonic index into the generated-name sequence
	dedupIndex            map[string][]byte // key: canonicalType \x00 rawValue -> generated variable name
	typeKeyScratch        []byte            // reused buffer for PrintTypeBytes

	// batched Input.Variables build (mondaytweaks.BatchExtractedVariablesJSON, optimized path
	// only). See appendExtractedVariableRaw in variables_extraction_optimized.go.
	batchVarsJSON bool
	varsBuf       []byte // per-document owned Input.Variables buffer; never reused across docs
	varsBufOwned  bool
}

func (v *variablesExtractionVisitor) EnterArgument(ref int) {
	if len(v.Ancestors) == 0 || v.Ancestors[0].Kind != ast.NodeKindOperationDefinition {
		return
	}

	for i := range v.Ancestors {
		if v.Ancestors[i].Kind == ast.NodeKindDirective {
			return // skip all directives in any case
		}
	}

	inputValueDefinition, ok := v.Walker.ArgumentInputValueDefinition(ref)
	if !ok {
		return
	}

	uploadsMapping, err := v.uploadFinder.FindUploads(v.operation, v.definition, v.operation.Input.Variables, ref, inputValueDefinition)
	if err != nil {
		v.StopWithInternalErr(err)
		return
	}
	v.uploadFinder.Reset()

	if v.operation.Arguments[ref].Value.Kind == ast.ValueKindVariable {
		if len(uploadsMapping) > 0 {
			v.uploadsPath = append(v.uploadsPath, uploadsMapping...)
		}
		return
	}

	valueBytes, err := v.operation.ValueToJSON(v.operation.Arguments[ref].Value)
	if err != nil {
		v.StopWithInternalErr(err)
		return
	}
	// dedupKey is populated only on the optimized path and reused below when a new
	// variable is registered, so identical (type,value) pairs collapse to one variable.
	var (
		dedupHit  bool
		dedupName []byte
		dedupKey  string
	)
	if v.optimize {
		if !v.preExistingNamesBuilt {
			v.buildPreExistingNames(v.Ancestors[0].Ref)
			v.preExistingNamesBuilt = true
		}
		dedupKey = v.typeValueDedupKey(inputValueDefinition, valueBytes)
		dedupName, dedupHit = v.dedupIndex[dedupKey]
	} else if exists, name, _ := v.variableExists(valueBytes, inputValueDefinition); exists {
		dedupName, dedupHit = name, true
	}
	if dedupHit {
		variable := ast.VariableValue{
			Name: v.operation.Input.AppendInputBytes(dedupName),
		}
		value := v.operation.AddVariableValue(variable)
		v.operation.Arguments[ref].Value.Kind = ast.ValueKindVariable
		v.operation.Arguments[ref].Value.Ref = value
		return
	}
	var variableNameBytes []byte
	if v.optimize {
		variableNameBytes = v.nextGeneratedVariableName()
	} else {
		variableNameBytes = v.operation.GenerateUnusedVariableDefinitionName(v.Ancestors[0].Ref)
	}
	// Register the new variable in Input.Variables. The batched path appends the member in
	// place (amortised O(1)); the default path uses sjson.SetRawBytes (O(N) per write). Both
	// place the new key at the end in first-occurrence order, so the resulting bytes match.
	// See mondaytweaks.BatchExtractedVariablesJSON.
	if v.batchVarsJSON {
		v.appendExtractedVariableRaw(variableNameBytes, valueBytes)
	} else {
		v.operation.Input.Variables, err = sjson.SetRawBytes(v.operation.Input.Variables, unsafebytes.BytesToString(variableNameBytes), valueBytes)
		if err != nil {
			v.StopWithInternalErr(err)
			return
		}
	}
	if v.optimize {
		v.dedupIndex[dedupKey] = variableNameBytes
	}

	if len(uploadsMapping) > 0 {
		// when we are extracting an input object into a variable and there were uploads inside
		// we have to update the upload path mapping to reflect the new extracted variable path
		for i := range uploadsMapping {
			if uploadsMapping[i].NewUploadPath != "" {
				// we alter a path only when upload was in a nested value
				// NewUploadPath, which returned from upload finder, is relative to the extracted argument "nested.f"
				variableNameString := string(variableNameBytes)
				// in order to replace file map values we prepend it with fully quilifying argument path in variables
				// e.g. variables.a.nested.f
				uploadsMapping[i].NewUploadPath = fmt.Sprintf("variables.%s.%s", variableNameString, uploadsMapping[i].NewUploadPath)
				// update variable name which holds an upload
				uploadsMapping[i].VariableName = variableNameString
			}
			v.uploadsPath = append(v.uploadsPath, uploadsMapping[i])
		}
	}

	v.extractedVariables = append(v.extractedVariables, variableNameBytes)
	v.extractedVariableTypeRefs = append(v.extractedVariableTypeRefs, v.definition.InputValueDefinitions[inputValueDefinition].Type)

	variable := ast.VariableValue{
		Name: v.operation.Input.AppendInputBytes(variableNameBytes),
	}

	v.operation.VariableValues = append(v.operation.VariableValues, variable)

	varRef := len(v.operation.VariableValues) - 1

	v.operation.Arguments[ref].Value.Ref = varRef
	v.operation.Arguments[ref].Value.Kind = ast.ValueKindVariable

	defRef, ok := v.ArgumentInputValueDefinition(ref)
	if !ok {
		return
	}

	defType := v.definition.InputValueDefinitions[defRef].Type

	importedDefType := v.importer.ImportType(defType, v.definition, v.operation)

	v.operation.VariableDefinitions = append(v.operation.VariableDefinitions, ast.VariableDefinition{
		VariableValue: ast.Value{
			Kind: ast.ValueKindVariable,
			Ref:  varRef,
		},
		Type: importedDefType,
	})

	newVariableRef := len(v.operation.VariableDefinitions) - 1

	v.operation.OperationDefinitions[v.Ancestors[0].Ref].VariableDefinitions.Refs =
		append(v.operation.OperationDefinitions[v.Ancestors[0].Ref].VariableDefinitions.Refs, newVariableRef)
	v.operation.OperationDefinitions[v.Ancestors[0].Ref].HasVariableDefinitions = true
}

func (v *variablesExtractionVisitor) EnterDocument(operation, definition *ast.Document) {
	v.operation, v.definition = operation, definition
	v.extractedVariables = v.extractedVariables[:0]
	v.extractedVariableTypeRefs = v.extractedVariableTypeRefs[:0]

	// The optimized path preserves byte-identical output only for single-operation
	// documents; multi-operation documents share generated names across operations
	// through the shared Input.Variables buffer, so they keep the original path.
	v.optimize = mondaytweaks.OptimizeVariablesExtraction && operation.NumOfOperationDefinitions() == 1
	v.preExistingNamesBuilt = false
	v.nameCursor = 0
	v.batchVarsJSON = v.optimize && mondaytweaks.BatchExtractedVariablesJSON
	v.varsBufOwned = false
	if v.optimize {
		if v.preExistingNames == nil {
			v.preExistingNames = make(map[string]struct{})
			v.dedupIndex = make(map[string][]byte)
		} else {
			clear(v.preExistingNames)
			clear(v.dedupIndex)
		}
	}
}

func (v *variablesExtractionVisitor) variableExists(variableValue []byte, inputValueDefinition int) (exists bool, name []byte, definition int) {
	_ = jsonparser.ObjectEach(v.operation.Input.Variables, func(key []byte, value []byte, dataType jsonparser.ValueType, offset int) error {
		if !v.extractedVariablesContainsKey(key, inputValueDefinition) {
			// skip variables that were not extracted but user defined
			return nil
		}
		if dataType == jsonparser.String {
			value = v.operation.Input.Variables[offset-len(value)-2 : offset]
		}
		if bytes.Equal(value, variableValue) {
			exists = true
			name = key
		}
		return nil
	})
	if exists {
		definition, exists = v.operation.VariableDefinitionByNameAndOperation(v.Ancestors[0].Ref, name)
	}
	return
}

func (v *variablesExtractionVisitor) extractedVariablesContainsKey(key []byte, inputValueDefinition int) bool {
	typeRef := v.definition.InputValueDefinitions[inputValueDefinition].Type
	for i := range v.extractedVariables {
		if bytes.Equal(v.extractedVariables[i], key) && v.definition.TypesAreEqualDeep(typeRef, v.extractedVariableTypeRefs[i]) {
			return true
		}
	}
	return false
}
