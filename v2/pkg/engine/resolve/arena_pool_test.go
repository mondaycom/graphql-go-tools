package resolve

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	arena "github.com/wundergraph/go-arena"
)

func TestArenaSizeClass(t *testing.T) {
	assert.Equal(t, 0, arenaSizeClass(0))
	assert.Equal(t, 0, arenaSizeClass(1))
	assert.Equal(t, 0, arenaSizeClass(32<<10))
	assert.Equal(t, 0, arenaSizeClass(64<<10))
	assert.Equal(t, 1, arenaSizeClass(64<<10+1))
	assert.Equal(t, 1, arenaSizeClass(128<<10))
	assert.Equal(t, 5, arenaSizeClass(2<<20))
	assert.Equal(t, arenaPoolNumClasses-1, arenaSizeClass(16<<20))
	assert.Equal(t, arenaPoolNumClasses-1, arenaSizeClass(64<<20))
}

func TestSizedArenaPool_RecordsPerRequestUsageNotLifetimePeak(t *testing.T) {
	p := newSizedArenaPool(0)

	// First request on key 1 allocates 2MB.
	item := p.Acquire(1)
	item.Arena.Alloc(2<<20, 8)
	p.Release(item)

	// Second request on key 1 allocates 1KB, possibly on the same arena whose
	// lifetime peak is 2MB. The recorded usage must be 1KB, not the peak.
	item = p.Acquire(1)
	item.Arena.Alloc(1<<10, 8)
	p.Release(item)

	s := p.sizes[uint64(1)]
	require.NotNil(t, s)
	assert.Equal(t, 2, s.count)
	// avg = (2MB + 1KB) / 2 — the second sample must not be inflated to 2MB.
	assert.Less(t, s.totalBytes/s.count, 1<<20+1<<10)
}

func TestSizedArenaPool_ClassMatchedReuse(t *testing.T) {
	p := newSizedArenaPool(0)

	// Grow an arena to ~2MB (class of 2MB) and release it.
	big := p.Acquire(1)
	big.Arena.Alloc(2<<20, 8)
	p.Release(big)

	// A small operation (no history → 1MB expected, class 4) may see the 2MB
	// arena in class 5 (one up) — that's allowed. But an operation whose
	// expected size is tiny must not receive it.
	for range 5 {
		it := p.Acquire(2)
		it.Arena.Alloc(1<<10, 8)
		p.Release(it)
	}
	// key 2 now has avg ~1KB → class 0. Class 0 lookup must not return the
	// 2MB arena parked in a higher class.
	small := p.Acquire(2)
	assert.LessOrEqual(t, small.Arena.Cap(), 128<<10)
	p.Release(small)
}

func TestSizedArenaPool_OversizeArenasSparselyPooled(t *testing.T) {
	p := newSizedArenaPool(0) // default oversize threshold: 4MB

	const oversize = 8 << 20
	items := make([]*poolItemRef, 0, 4)
	for i := range 4 {
		it := p.Acquire(uint64(100 + i))
		it.Arena.Alloc(oversize, 8)
		items = append(items, &poolItemRef{item: it})
	}
	for _, ref := range items {
		p.Release(ref.item)
	}

	// Only maxOversizePooledPerClass oversize arenas may be parked; the rest
	// must have been released (capacity freed).
	total, maxCap, count := p.retainedBytes()
	assert.LessOrEqual(t, count, maxOversizePooledPerClass)
	assert.LessOrEqual(t, total, maxOversizePooledPerClass*(oversize+oversize/2))
	assert.GreaterOrEqual(t, maxCap, oversize)
}

// poolItemRef keeps strong references so the weak-pointer pool cannot be
// trimmed by a concurrent GC mid-test.
type poolItemRef struct {
	item *arena.PoolItem
}
