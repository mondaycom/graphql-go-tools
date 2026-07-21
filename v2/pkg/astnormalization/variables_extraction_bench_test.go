package astnormalization

import (
	"fmt"
	"strings"
	"testing"

	"github.com/wundergraph/graphql-go-tools/v2/pkg/asttransform"
	"github.com/wundergraph/graphql-go-tools/v2/pkg/astvisitor"
	"github.com/wundergraph/graphql-go-tools/v2/pkg/internal/unsafeparser"
	"github.com/wundergraph/graphql-go-tools/v2/pkg/mondaytweaks"
	"github.com/wundergraph/graphql-go-tools/v2/pkg/operationreport"
)

// benchExtractionSchema models the monday.com bulk-mutation shape:
// changeColumnValue takes 4 scalar args (2 repeat across the batch, 2 are unique
// per aliased field) and returns an object with a selectable field.
const benchExtractionSchema = `
schema { query: Query mutation: Mutation }
type Query { hello: String }
type Item { id: ID }
type Mutation {
	changeColumnValue(boardId: ID, itemId: ID, columnId: String, value: String): Item
}
`

// buildAliasedBatchMutation reproduces the monday.com bulk shape:
// one named mutation with N aliased changeColumnValue root fields, all args
// inline. boardId + columnId repeat (dedup candidates); itemId + value are unique.
func buildAliasedBatchMutation(n int) string {
	var b strings.Builder
	b.WriteString("mutation BulkChangeColumnValue {\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b,
			"  u%d: changeColumnValue(boardId: 5397435412, itemId: %d, columnId: \"numbers_mkkbc1yz\", value: \"%d\") { id }\n",
			i, 6000000000+i, i*100)
	}
	b.WriteString("}")
	return b.String()
}

// benchExtractionSchemaWithUpload adds an Upload scalar so uploads.FindUploads engages its full
// per-argument astjson.ParseBytes pass (NodeByName("Upload") succeeds), matching the federated
// router schema. Used to measure the overhead mondaytweaks.DisableUploadFinding removes.
const benchExtractionSchemaWithUpload = benchExtractionSchema + "\nscalar Upload\n"

func benchmarkExtraction(b *testing.B, n int) {
	benchmarkExtractionSchema(b, benchExtractionSchema, n)
}

func benchmarkExtractionSchema(b *testing.B, schema string, n int) {
	def := unsafeparser.ParseGraphqlDocumentString(schema)
	if err := asttransform.MergeDefinitionWithBaseSchema(&def); err != nil {
		b.Fatal(err)
	}
	op := buildAliasedBatchMutation(n)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		opDoc := unsafeparser.ParseGraphqlDocumentString(op)
		report := operationreport.Report{}
		walker := astvisitor.NewWalker(48)
		extractVariables(&walker)
		b.StartTimer()

		walker.Walk(&opDoc, &def, &report)
		if report.HasErrors() {
			b.Fatal(report.Error())
		}
	}
}

func BenchmarkVariablesExtraction_AliasedBatch(b *testing.B) {
	for _, n := range []int{10, 25, 50, 100, 200} {
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			benchmarkExtraction(b, n)
		})
	}
}

// BenchmarkVariablesExtraction_UploadFinding measures the per-argument uploads.FindUploads pass
// on an Upload-bearing schema (as the router schema is) for the aliased-batch shape, comparing
// the default (mondaytweaks.DisableUploadFinding on) against the upstream pass. Run with
// -benchmem to see the astjson.ParseBytes allocations the flag removes.
func BenchmarkVariablesExtraction_UploadFinding(b *testing.B) {
	origDisableUploadFinding := mondaytweaks.DisableUploadFinding.Load()
	b.Cleanup(func() { mondaytweaks.DisableUploadFinding.Store(origDisableUploadFinding) })

	for _, n := range []int{25, 50, 100, 200} {
		mondaytweaks.DisableUploadFinding.Store(false)
		b.Run(fmt.Sprintf("finding/N=%d", n), func(b *testing.B) { benchmarkExtractionSchema(b, benchExtractionSchemaWithUpload, n) })

		mondaytweaks.DisableUploadFinding.Store(true)
		b.Run(fmt.Sprintf("skipped/N=%d", n), func(b *testing.B) { benchmarkExtractionSchema(b, benchExtractionSchemaWithUpload, n) })
	}
}
