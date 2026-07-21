package astnormalization

import (
	"fmt"
	"testing"

	"github.com/wundergraph/graphql-go-tools/v2/pkg/asttransform"
	"github.com/wundergraph/graphql-go-tools/v2/pkg/internal/unsafeparser"
	"github.com/wundergraph/graphql-go-tools/v2/pkg/operationreport"
)

// buildNormalizationOptions mirrors the production router's buildNormalizationOptions
// in cosmo/router/core/operation_processor.go (defer disabled, the common path).
func buildProductionNormalizerOpts() []Option {
	return []Option{
		WithRemoveNotMatchingOperationDefinitions(),
		WithInlineFragmentSpreads(),
		WithRemoveFragmentDefinitions(),
		WithRemoveUnusedVariables(),
	}
}

// BenchmarkFullNormalization_AliasedBatch measures the static normalization
// pipeline (production options, no variable extraction) on the batch-mutation
// shape that was observed to take >2s in production planning.
//
// Run with:
//
//	go test -run=^$ -bench=BenchmarkFullNormalization_AliasedBatch -benchmem ./v2/pkg/astnormalization/
func BenchmarkFullNormalization_AliasedBatch(b *testing.B) {
	for _, n := range []int{10, 25, 50, 100, 200} {
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			benchmarkFullNormalization(b, n)
		})
	}
}

// BenchmarkFullNormalizationWithExtract_AliasedBatch adds variable extraction
// (the VariablesNormalizer step that runs after static normalization), isolating
// the combined cost that the router pays per request for this mutation shape.
func BenchmarkFullNormalizationWithExtract_AliasedBatch(b *testing.B) {
	for _, n := range []int{10, 25, 50, 100, 200} {
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			benchmarkFullNormalizationWithExtract(b, n)
		})
	}
}

func benchmarkFullNormalization(b *testing.B, n int) {
	def := unsafeparser.ParseGraphqlDocumentString(benchExtractionSchema)
	if err := asttransform.MergeDefinitionWithBaseSchema(&def); err != nil {
		b.Fatal(err)
	}
	op := buildAliasedBatchMutation(n)
	normalizer := NewWithOpts(buildProductionNormalizerOpts()...)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		opDoc := unsafeparser.ParseGraphqlDocumentString(op)
		report := operationreport.Report{}
		b.StartTimer()

		normalizer.NormalizeNamedOperation(&opDoc, &def, []byte("BulkChangeColumnValue"), &report)
		if report.HasErrors() {
			b.Fatal(report.Error())
		}
	}
}

func benchmarkFullNormalizationWithExtract(b *testing.B, n int) {
	def := unsafeparser.ParseGraphqlDocumentString(benchExtractionSchema)
	if err := asttransform.MergeDefinitionWithBaseSchema(&def); err != nil {
		b.Fatal(err)
	}
	op := buildAliasedBatchMutation(n)
	staticNormalizer := NewWithOpts(buildProductionNormalizerOpts()...)
	variablesNormalizer := NewVariablesNormalizer()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		opDoc := unsafeparser.ParseGraphqlDocumentString(op)
		report := operationreport.Report{}
		b.StartTimer()

		staticNormalizer.NormalizeNamedOperation(&opDoc, &def, nil, &report)
		if report.HasErrors() {
			b.Fatal(report.Error())
		}
		variablesNormalizer.NormalizeOperation(&opDoc, &def, &report)
		if report.HasErrors() {
			b.Fatal(report.Error())
		}
	}
}
