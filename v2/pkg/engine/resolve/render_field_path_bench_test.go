package resolve

// BenchmarkRenderFieldPath_CacheRenderFieldPath benchmarks the CacheRenderFieldPath
// optimization, comparing flag-off (original pool.BytesBuffer path) vs flag-on
// (reusable per-Resolvable scratch buffer + zero-alloc map lookup).
//
// Two benchmark pairs target the two distinct patterns:
//
//  1. _renderFieldPath — raw renderFieldPath() call, representing recordObjectTypeStats /
//     walkArray / renderFieldValue which always materialise a string.
//
//  2. _recordFieldReachedLookup — the lookup-only path in recordFieldReached where the
//     path already exists in typeNameStats.  With the flag on, the zero-alloc
//     m[string(b)] trick fires and no heap string is produced; flag off allocates one
//     string per call.

import (
	"testing"

	"github.com/wundergraph/graphql-go-tools/v2/pkg/ast"
	"github.com/wundergraph/graphql-go-tools/v2/pkg/fastjsonext"
	"github.com/wundergraph/graphql-go-tools/v2/pkg/mondaytweaks"
)

// newBenchResolvable returns a Resolvable positioned at Query.teams.name with
// EnableCostControl enabled.
func newBenchResolvable(t testing.TB) *Resolvable {
	t.Helper()
	ctx := &Context{}
	r := NewResolvable(nil, ResolvableOptions{EnableCostControl: true})
	if err := r.Init(ctx, nil, ast.OperationTypeQuery); err != nil {
		t.Fatal(err)
	}
	// Simulate being inside Query → teams → name.
	r.path = append(r.path,
		fastjsonext.PathElement{Name: "teams"},
		fastjsonext.PathElement{Name: "name"},
	)
	return r
}

// BenchmarkRenderFieldPath_flagOff measures renderFieldPath() with CacheRenderFieldPath=false.
// Original path: pool.BytesBuffer.Get() + buf.String() → 1 alloc per call.
func BenchmarkRenderFieldPath_flagOff(b *testing.B) {
	mondaytweaks.CacheRenderFieldPath.Store(false)
	b.Cleanup(func() { mondaytweaks.CacheRenderFieldPath.Store(true) })

	r := newBenchResolvable(b)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = r.renderFieldPath()
	}
}

// BenchmarkRenderFieldPath_flagOn measures renderFieldPath() with CacheRenderFieldPath=true.
// New path: reuses r.fieldPathBuf (no pool), one alloc for the returned string.
func BenchmarkRenderFieldPath_flagOn(b *testing.B) {
	mondaytweaks.CacheRenderFieldPath.Store(true)

	r := newBenchResolvable(b)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = r.renderFieldPath()
	}
}

// BenchmarkRecordFieldReachedLookup_flagOff benchmarks recordFieldReached when the path
// already exists in typeNameStats (lookup-only, insert branch not taken) with flag off.
// The original code still calls renderFieldPath() → 1 alloc even though no insert is done.
func BenchmarkRecordFieldReachedLookup_flagOff(b *testing.B) {
	mondaytweaks.CacheRenderFieldPath.Store(false)
	b.Cleanup(func() { mondaytweaks.CacheRenderFieldPath.Store(true) })

	r := newBenchResolvable(b)

	// Pre-insert the path so the typeNameStats lookup hits the "exists" branch.
	mondaytweaks.CacheRenderFieldPath.Store(true)
	r.buildFieldPathBuf()
	r.typeNameStats[string(r.fieldPathBuf)] = TypeNameStats{}
	mondaytweaks.CacheRenderFieldPath.Store(false)

	field := &Field{
		Value: &String{Path: []string{"name"}},
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		// Remove from reachedFields so the dedup guard does not short-circuit.
		delete(r.reachedFields, field)
		r.recordFieldReached(nil, field)
	}
}

// BenchmarkRecordFieldReachedLookup_flagOn benchmarks the same pattern with flag on.
// Zero allocs for the lookup branch: the Go compiler's m[string(b)] optimisation
// elides the heap allocation when the key already exists.
func BenchmarkRecordFieldReachedLookup_flagOn(b *testing.B) {
	mondaytweaks.CacheRenderFieldPath.Store(true)

	r := newBenchResolvable(b)

	// Pre-insert so we hit the lookup-only branch.
	r.buildFieldPathBuf()
	r.typeNameStats[string(r.fieldPathBuf)] = TypeNameStats{}

	field := &Field{
		Value: &String{Path: []string{"name"}},
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		delete(r.reachedFields, field)
		r.recordFieldReached(nil, field)
	}
}
