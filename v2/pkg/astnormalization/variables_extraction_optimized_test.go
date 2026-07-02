package astnormalization

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wundergraph/graphql-go-tools/v2/pkg/astprinter"
	"github.com/wundergraph/graphql-go-tools/v2/pkg/asttransform"
	"github.com/wundergraph/graphql-go-tools/v2/pkg/astvisitor"
	"github.com/wundergraph/graphql-go-tools/v2/pkg/internal/unsafeparser"
	"github.com/wundergraph/graphql-go-tools/v2/pkg/mondaytweaks"
	"github.com/wundergraph/graphql-go-tools/v2/pkg/operationreport"
)

// extractOnce parses definition+operation, runs only the variable-extraction walker, and
// returns the printed operation AST together with the resulting Input.Variables JSON.
func extractOnce(t *testing.T, definition, operation string) (printedAST, variables string) {
	t.Helper()

	def := unsafeparser.ParseGraphqlDocumentString(definition)
	require.NoError(t, asttransform.MergeDefinitionWithBaseSchema(&def))

	op := unsafeparser.ParseGraphqlDocumentString(operation)
	report := operationreport.Report{}
	walker := astvisitor.NewWalker(48)
	extractVariables(&walker)
	walker.Walk(&op, &def, &report)
	require.Falsef(t, report.HasErrors(), "unexpected normalization error: %s", report.Error())

	printed, err := astprinter.PrintString(&op)
	require.NoError(t, err)
	return printed, string(op.Input.Variables)
}

// TestVariablesExtraction_OptimizedMatchesOriginal asserts the optimized extraction path
// produces byte-identical output (printed AST + variables JSON) to the original path across
// a corpus that stresses the parts the optimization touches: name generation across the
// 26/52 length boundaries, pre-existing user-variable name collisions, and value/type
// deduplication.
// extractionCorpusEntry is a single-operation extraction case shared by the differential
// tests. Every entry must be a single-operation document so the optimized path engages.
type extractionCorpusEntry struct {
	name       string
	definition string
	operation  string
}

// extractionCorpus stresses the parts the optimization touches: name generation across the
// 26/52 length boundaries, pre-existing user-variable name collisions, value/type
// deduplication, and nested inputs.
func extractionCorpus() []extractionCorpusEntry {
	return []extractionCorpusEntry{
		{name: "single inline arg", definition: benchExtractionSchema, operation: buildAliasedBatchMutation(1)},
		{name: "batch N=5", definition: benchExtractionSchema, operation: buildAliasedBatchMutation(5)},
		{name: "batch N=26 (single-letter boundary)", definition: benchExtractionSchema, operation: buildAliasedBatchMutation(26)},
		{name: "batch N=27 (into two-letter names)", definition: benchExtractionSchema, operation: buildAliasedBatchMutation(27)},
		{name: "batch N=53 (into three-letter names)", definition: benchExtractionSchema, operation: buildAliasedBatchMutation(53)},
		{name: "batch N=200", definition: benchExtractionSchema, operation: buildAliasedBatchMutation(200)},
		{
			name:       "dedup identical scalars",
			definition: sameVariableExtraction,
			operation: `mutation Foo {
				bar(string: "foo")
				bar(string: "foo")
				baz(int: 1)
				baz(int: 1)
			}`,
		},
		{
			name:       "dedup with pre-existing user variable named a",
			definition: sameVariableExtraction,
			operation: `mutation Foo($another: String) {
				another: bar(string: $another)
				bar(string: "foo")
				bar(string: "foo")
			}`,
		},
		{
			name:       "pre-existing names a and b force cursor skip",
			definition: benchExtractionSchema,
			operation: `mutation M($a: ID, $b: ID) {
				x: change_column_value(board_id: $a, item_id: 2, column_id: "c", value: "1") { id }
				y: change_column_value(board_id: $b, item_id: 3, column_id: "c", value: "2") { id }
			}`,
		},
		{
			name:       "nested inputs and lists",
			definition: sameVariableExtraction,
			operation: `mutation Foo {
				foo(input: {input: {string: "foo"}})
				foo(input: {inputs: [{string: "foo"}, {string: "bar"}]})
				foo(input: {ints: [1, 2]})
				foo(input: {ints: [1, 2]})
			}`,
		},
	}
}

func TestVariablesExtraction_OptimizedMatchesOriginal(t *testing.T) {
	original := mondaytweaks.OptimizeVariablesExtraction
	t.Cleanup(func() { mondaytweaks.OptimizeVariablesExtraction = original })

	for _, c := range extractionCorpus() {
		t.Run(c.name, func(t *testing.T) {
			mondaytweaks.OptimizeVariablesExtraction = false
			astOrig, varsOrig := extractOnce(t, c.definition, c.operation)

			mondaytweaks.OptimizeVariablesExtraction = true
			astOpt, varsOpt := extractOnce(t, c.definition, c.operation)

			assert.Equal(t, astOrig, astOpt, "printed AST diverges between original and optimized paths")
			assert.Equal(t, varsOrig, varsOpt, "variables JSON diverges between original and optimized paths")
		})
	}
}

// TestVariablesExtraction_MultiOperationFallsBack verifies multi-operation documents (where
// the optimized path is intentionally disabled) still match the original path exactly.
func TestVariablesExtraction_MultiOperationFallsBack(t *testing.T) {
	original := mondaytweaks.OptimizeVariablesExtraction
	t.Cleanup(func() { mondaytweaks.OptimizeVariablesExtraction = original })

	const op = `
		mutation A { bar(string: "foo") baz(int: 1) }
		mutation B { bar(string: "foo") baz(int: 2) }`

	mondaytweaks.OptimizeVariablesExtraction = false
	astOrig, varsOrig := extractOnce(t, sameVariableExtraction, op)

	mondaytweaks.OptimizeVariablesExtraction = true
	astOpt, varsOpt := extractOnce(t, sameVariableExtraction, op)

	assert.Equal(t, astOrig, astOpt)
	assert.Equal(t, varsOrig, varsOpt)
}

// TestVariablesExtraction_BatchedMatchesSemantically asserts the batched Input.Variables
// build (mondaytweaks.BatchExtractedVariablesJSON) is semantically equivalent to the
// per-variable sjson path: the printed operation AST is byte-identical (both paths generate
// the same variable names in the same order and only differ in how the variables object is
// serialised) and the variables object is deep-equal. It is intentionally NOT byte-identical:
// sjson prepends new keys while the batched build appends them, so the two objects are
// key-reordered. Order is irrelevant downstream (RemapVariables canonicalises it before the
// operation hash), which is what makes shipping the batched build safe.
func TestVariablesExtraction_BatchedMatchesSemantically(t *testing.T) {
	origOptimize := mondaytweaks.OptimizeVariablesExtraction
	origBatch := mondaytweaks.BatchExtractedVariablesJSON
	t.Cleanup(func() {
		mondaytweaks.OptimizeVariablesExtraction = origOptimize
		mondaytweaks.BatchExtractedVariablesJSON = origBatch
	})

	// Both configurations run the optimized name/dedup path; only the Input.Variables writer
	// differs (sjson vs in-place append).
	mondaytweaks.OptimizeVariablesExtraction = true

	for _, c := range extractionCorpus() {
		t.Run(c.name, func(t *testing.T) {
			mondaytweaks.BatchExtractedVariablesJSON = false
			astSjson, varsSjson := extractOnce(t, c.definition, c.operation)

			mondaytweaks.BatchExtractedVariablesJSON = true
			astBatched, varsBatched := extractOnce(t, c.definition, c.operation)

			assert.Equal(t, astSjson, astBatched, "printed AST diverges between sjson and batched paths")
			assert.JSONEq(t, varsSjson, varsBatched, "variables object diverges (beyond key order) between sjson and batched paths")
		})
	}
}

// BenchmarkVariablesExtraction_OptimizedVsOriginal runs the batch shape under both paths so
// the speedup is directly comparable.
func BenchmarkVariablesExtraction_OptimizedVsOriginal(b *testing.B) {
	original := mondaytweaks.OptimizeVariablesExtraction
	b.Cleanup(func() { mondaytweaks.OptimizeVariablesExtraction = original })

	for _, n := range []int{25, 50, 100, 200} {
		mondaytweaks.OptimizeVariablesExtraction = false
		b.Run(fmt.Sprintf("original/N=%d", n), func(b *testing.B) { benchmarkExtraction(b, n) })

		mondaytweaks.OptimizeVariablesExtraction = true
		b.Run(fmt.Sprintf("optimized/N=%d", n), func(b *testing.B) { benchmarkExtraction(b, n) })
	}
}

// BenchmarkVariablesExtraction_SjsonVsBatched isolates the Input.Variables writer: both arms
// run the optimized name/dedup path, so the delta is the per-variable sjson.SetRawBytes
// (O(N^2) bytes copied) versus the in-place append (O(N) amortised). Run with -benchmem to see
// the allocation reduction that motivated BatchExtractedVariablesJSON.
func BenchmarkVariablesExtraction_SjsonVsBatched(b *testing.B) {
	origOptimize := mondaytweaks.OptimizeVariablesExtraction
	origBatch := mondaytweaks.BatchExtractedVariablesJSON
	b.Cleanup(func() {
		mondaytweaks.OptimizeVariablesExtraction = origOptimize
		mondaytweaks.BatchExtractedVariablesJSON = origBatch
	})

	mondaytweaks.OptimizeVariablesExtraction = true

	for _, n := range []int{25, 50, 100, 200} {
		mondaytweaks.BatchExtractedVariablesJSON = false
		b.Run(fmt.Sprintf("sjson/N=%d", n), func(b *testing.B) { benchmarkExtraction(b, n) })

		mondaytweaks.BatchExtractedVariablesJSON = true
		b.Run(fmt.Sprintf("batched/N=%d", n), func(b *testing.B) { benchmarkExtraction(b, n) })
	}
}
