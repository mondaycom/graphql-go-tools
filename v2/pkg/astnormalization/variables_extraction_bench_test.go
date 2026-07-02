package astnormalization

import (
	"fmt"
	"strings"
	"testing"

	"github.com/wundergraph/graphql-go-tools/v2/pkg/asttransform"
	"github.com/wundergraph/graphql-go-tools/v2/pkg/astvisitor"
	"github.com/wundergraph/graphql-go-tools/v2/pkg/internal/unsafeparser"
	"github.com/wundergraph/graphql-go-tools/v2/pkg/operationreport"
)

// benchExtractionSchema models the monday.com bulk-mutation shape:
// change_column_value takes 4 scalar args (2 repeat across the batch, 2 are unique
// per aliased field) and returns an object with a selectable field.
const benchExtractionSchema = `
schema { query: Query mutation: Mutation }
type Query { hello: String }
type Item { id: ID }
type Mutation {
	change_column_value(board_id: ID, item_id: ID, column_id: String, value: String): Item
}
`

// buildAliasedBatchMutation reproduces the Google-Apps-Script bulk shape:
// one anonymous mutation with N aliased change_column_value root fields, all args
// inline. board_id + column_id repeat (dedup candidates); item_id + value are unique.
func buildAliasedBatchMutation(n int) string {
	var b strings.Builder
	b.WriteString("mutation {\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b,
			"  u%d: change_column_value(board_id: 5397435412, item_id: %d, column_id: \"numbers_mkkbc1yz\", value: \"%d\") { id }\n",
			i, 6000000000+i, i*100)
	}
	b.WriteString("}")
	return b.String()
}

func benchmarkExtraction(b *testing.B, n int) {
	def := unsafeparser.ParseGraphqlDocumentString(benchExtractionSchema)
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
