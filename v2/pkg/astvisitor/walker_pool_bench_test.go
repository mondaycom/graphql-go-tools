package astvisitor_test

// BenchmarkWalkerPoolVsNew measures the per-call allocation cost of
// NewWalkerWithID vs WalkerFromPoolWithID + Release.
//
// Each benchmark iteration creates a walker, registers a trivial EnterField
// visitor, walks a small operation+schema document (same as BenchmarkVisitor),
// and discards the walker.  This mirrors the four per-request planning sites
// (addRequiredFields, areRequiredFieldsProvided, collectPath, getKeyPaths).
//
// Expected outcome: the pool path sheds the 4 per-call slice allocations that
// NewWalkerWithID makes (Ancestors, Path, TypeDefinitions, deferred), so
// allocs/op and B/op should be materially lower on the pool path.

import (
	"testing"

	"github.com/wundergraph/graphql-go-tools/v2/pkg/astvisitor"
	"github.com/wundergraph/graphql-go-tools/v2/pkg/internal/unsafeparser"
	"github.com/wundergraph/graphql-go-tools/v2/pkg/operationreport"
)

// trivialFieldVisitor is the lightest possible visitor — it registers one
// callback and does nothing in it, so the walker overhead dominates.
type trivialFieldVisitor struct{}

func (t *trivialFieldVisitor) EnterField(_ int) {}

func BenchmarkWalkerNew(b *testing.B) {
	definition := unsafeparser.ParseGraphqlDocumentString(testDefinition)
	operation := unsafeparser.ParseGraphqlDocumentString(testOperation)
	report := operationreport.Report{}
	visitor := &trivialFieldVisitor{}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		// Mirrors NewWalkerWithID(8, "x") call sites.
		w := astvisitor.NewWalkerWithID(8, "BenchmarkNew")
		w.RegisterEnterFieldVisitor(visitor)
		report.Reset()
		w.Walk(&operation, &definition, &report)
		// walker is discarded (no Release) — same as production sites before this change
	}
}

func BenchmarkWalkerPool(b *testing.B) {
	definition := unsafeparser.ParseGraphqlDocumentString(testDefinition)
	operation := unsafeparser.ParseGraphqlDocumentString(testOperation)
	report := operationreport.Report{}
	visitor := &trivialFieldVisitor{}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		// Mirrors the new pool path: WalkerFromPoolWithID + defer Release.
		w := astvisitor.WalkerFromPoolWithID("BenchmarkPool")
		w.RegisterEnterFieldVisitor(visitor)
		report.Reset()
		w.Walk(&operation, &definition, &report)
		w.Release()
	}
}
