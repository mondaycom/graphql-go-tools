package resolve

import (
	"math/rand"
	"sync"
	"testing"

	arena "github.com/wundergraph/go-arena"
)

// retentionWorkload models the response-size distribution of the main (public,
// high-cardinality) router deployment: the overwhelming majority of operations
// resolve small responses, with a long tail of large and rare huge ones.
// Peaks are arena peak bytes per request (astjson tree + merged subgraph data),
// not wire sizes.
type retentionOpClass struct {
	weight int // relative frequency
	peak   int // arena bytes the operation allocates
}

var retentionWorkload = []retentionOpClass{
	{weight: 850, peak: 32 << 10},  // small queries
	{weight: 100, peak: 256 << 10}, // medium
	{weight: 45, peak: 2 << 20},    // large
	{weight: 5, peak: 16 << 20},    // huge, rare
}

const (
	retentionOpCardinality = 2048 // distinct operation hashes
	retentionConcurrency   = 64   // concurrent in-flight requests
	retentionAllocChunk    = 512  // simulate many small astjson node allocs
)

// buildRetentionOps deterministically assigns each of the distinct operation
// keys to a workload class proportionally to class weight.
func buildRetentionOps() []retentionOpClass {
	totalWeight := 0
	for _, c := range retentionWorkload {
		totalWeight += c.weight
	}
	ops := make([]retentionOpClass, retentionOpCardinality)
	for i := range ops {
		w := (i * totalWeight) / retentionOpCardinality
		acc := 0
		for _, c := range retentionWorkload {
			acc += c.weight
			if w < acc {
				ops[i] = c
				break
			}
		}
	}
	// Shuffle deterministically so key order doesn't correlate with size.
	r := rand.New(rand.NewSource(42))
	r.Shuffle(len(ops), func(i, j int) { ops[i], ops[j] = ops[j], ops[i] })
	return ops
}

// retentionTracker records every distinct PoolItem observed and its capacity
// at release time. Holding strong references prevents the weak-pointer pool
// from being trimmed by GC, which makes the benchmark deterministic and
// equivalent to the steady-state working set under sustained load (in
// production, sustained traffic keeps pooled arenas alive between GC cycles).
type retentionTracker struct {
	mu    sync.Mutex
	items map[*arena.PoolItem]int
}

func newRetentionTracker() *retentionTracker {
	return &retentionTracker{items: make(map[*arena.PoolItem]int)}
}

func (t *retentionTracker) observe(item *arena.PoolItem) {
	cap := item.Arena.Cap()
	t.mu.Lock()
	t.items[item] = cap
	t.mu.Unlock()
}

func (t *retentionTracker) report(b *testing.B) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var sum, max int
	for _, c := range t.items {
		sum += c
		if c > max {
			max = c
		}
	}
	b.ReportMetric(float64(sum)/(1<<20), "retained-MB")
	b.ReportMetric(float64(max)/(1<<20), "max-arena-MB")
	b.ReportMetric(float64(len(t.items)), "arenas")
}

func runRetentionBenchmark(b *testing.B, pool arenaPool) {
	ops := buildRetentionOps()
	tracker := newRetentionTracker()

	b.ReportAllocs()
	b.ResetTimer()

	var wg sync.WaitGroup
	iterations := b.N
	perWorker := iterations / retentionConcurrency
	if perWorker == 0 {
		perWorker = 1
	}
	for w := range retentionConcurrency {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			r := rand.New(rand.NewSource(seed))
			for i := 0; i < perWorker; i++ {
				keyIdx := r.Intn(retentionOpCardinality)
				op := ops[keyIdx]
				item := pool.Acquire(uint64(keyIdx) + 1)
				// Simulate the resolver allocating the astjson tree in many
				// small chunks up to the operation's peak.
				a := item.Arena
				for allocated := 0; allocated < op.peak; allocated += retentionAllocChunk {
					_ = a.Alloc(retentionAllocChunk, 8)
				}
				tracker.observe(item)
				pool.Release(item)
			}
		}(int64(w))
	}
	wg.Wait()

	b.StopTimer()
	// For the size-classed pool, dropped arenas have already freed their
	// buffers, so ask the pool for what is actually parked. The upstream pool
	// never drops or frees, so the historical tracker sum is exact for it.
	if sized, ok := pool.(*sizedArenaPool); ok {
		total, maxCap, count := sized.retainedBytes()
		b.ReportMetric(float64(total)/(1<<20), "retained-MB")
		b.ReportMetric(float64(maxCap)/(1<<20), "max-arena-MB")
		b.ReportMetric(float64(count), "arenas")
		return
	}
	tracker.report(b)
}

func Benchmark_ArenaPoolRetention_Upstream(b *testing.B) {
	runRetentionBenchmark(b, arena.NewArenaPool())
}

func Benchmark_ArenaPoolRetention_Sized(b *testing.B) {
	runRetentionBenchmark(b, newSizedArenaPool(0))
}
