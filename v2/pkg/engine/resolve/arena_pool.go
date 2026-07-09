package resolve

import (
	"math/bits"
	"sync"
	"weak"

	arena "github.com/wundergraph/go-arena"
)

// sizedArenaPool is a drop-in replacement for arena.Pool that bounds the
// memory retained by pooled arenas. It fixes two retention pathologies of the
// upstream pool observed under high-cardinality, bimodally-sized traffic:
//
//  1. Upstream Acquire pops any pooled arena regardless of size. Because a
//     monotonic arena never shrinks (Reset keeps capacity), every pooled arena
//     eventually serves one huge response and stays that large forever. The
//     steady-state working set converges to concurrency × largest-response
//     size. sizedArenaPool keeps arenas in power-of-two size classes and hands
//     out an arena whose capacity matches the operation's expected usage, so
//     small operations circulate small arenas.
//
//  2. Upstream Release records Arena.Peak() — a lifetime high-water mark that
//     survives Reset — into the per-key sizing stats. One huge request
//     permanently inflates the recorded size of every key that arena serves
//     afterwards. sizedArenaPool records Arena.Len() (bytes allocated since
//     the last Reset), i.e. the actual per-request usage.
//
// Arenas whose capacity exceeds maxRetainedArenaBytes are released outright
// instead of pooled, so a rare giant response cannot park its capacity in the
// pool. Like upstream, pooled items are held via weak pointers, letting the GC
// trim the pool under memory pressure.
// arenaPool is the pooling contract used by the Resolver for request arenas,
// satisfied by both arena.Pool and sizedArenaPool.
type arenaPool interface {
	Acquire(key uint64) *arena.PoolItem
	Release(item *arena.PoolItem)
}

// newRequestArenaPool returns the arena pool implementation selected by the
// resolver options: the size-classed retention-bounded pool by default, or the
// upstream arena.Pool when DisableSizedArenaPool is set.
func newRequestArenaPool(options ResolverOptions) arenaPool {
	if options.DisableSizedArenaPool {
		return arena.NewArenaPool()
	}
	return newSizedArenaPool(options.ArenaPoolMaxRetainedArenaBytes)
}

type sizedArenaPool struct {
	mu      sync.Mutex
	classes [arenaPoolNumClasses][]weak.Pointer[arena.PoolItem]
	sizes   map[uint64]*arenaKeyStats

	// maxRetainedArenaBytes is the capacity above which an arena is not
	// returned to the pool on Release.
	maxRetainedArenaBytes int
}

// arenaKeyStats tracks a rolling average of per-request arena usage across the
// last 50 requests for one operation key (same scheme as upstream arena.Pool).
type arenaKeyStats struct {
	count      int
	totalBytes int
}

const (
	// arenaPoolMinClassShift: capacities up to 64KB share the smallest class.
	arenaPoolMinClassShift = 16
	// arenaPoolMaxClassShift: capacities above 16MB share the largest class
	// (only reachable when maxRetainedArenaBytes is raised above the default).
	arenaPoolMaxClassShift = 24
	arenaPoolNumClasses    = arenaPoolMaxClassShift - arenaPoolMinClassShift + 1

	// defaultArenaPoolMaxRetainedArenaBytes is the capacity above which an
	// arena is considered oversize. Responses larger than this still resolve
	// fine — the arena grows as needed — but only a handful of oversize arenas
	// are recycled (see maxOversizePooledPerClass); the rest are freed on
	// Release.
	defaultArenaPoolMaxRetainedArenaBytes = 4 << 20

	// maxOversizePooledPerClass bounds how many oversize arenas each size
	// class keeps. A small number lets recurring large operations reuse a
	// grown arena (avoiding a multi-MB re-allocation per request) while
	// keeping worst-case pooled capacity of the oversize classes to
	// maxOversizePooledPerClass × Σ(class sizes) instead of
	// concurrency × largest-response.
	maxOversizePooledPerClass = 2

	// defaultExpectedArenaSize is used for keys with no recorded history,
	// matching the upstream pool's 1MB default.
	defaultExpectedArenaSize = 1 << 20
)

func newSizedArenaPool(maxRetainedArenaBytes int) *sizedArenaPool {
	if maxRetainedArenaBytes <= 0 {
		maxRetainedArenaBytes = defaultArenaPoolMaxRetainedArenaBytes
	}
	return &sizedArenaPool{
		sizes:                 make(map[uint64]*arenaKeyStats),
		maxRetainedArenaBytes: maxRetainedArenaBytes,
	}
}

// arenaSizeClass maps a byte size to a power-of-two class index, clamped to
// [0, arenaPoolNumClasses-1].
func arenaSizeClass(size int) int {
	if size <= 0 {
		return 0
	}
	shift := bits.Len(uint(size - 1)) // ceil(log2(size))
	if shift < arenaPoolMinClassShift {
		return 0
	}
	if shift > arenaPoolMaxClassShift {
		return arenaPoolNumClasses - 1
	}
	return shift - arenaPoolMinClassShift
}

// Acquire returns an arena sized for the expected usage of the given
// operation key: a pooled arena from the matching (or next larger) size class,
// or a new arena pre-sized to the key's rolling-average usage.
func (p *sizedArenaPool) Acquire(key uint64) *arena.PoolItem {
	p.mu.Lock()

	expected := defaultExpectedArenaSize
	if s, ok := p.sizes[key]; ok && s.count > 0 {
		expected = s.totalBytes / s.count
	}

	cls := arenaSizeClass(expected)
	// Exact class first, then one class larger. Classes further up would hand
	// a much larger arena to a small operation and keep big capacity hot;
	// classes below would just grow back to expected.
	for c := cls; c <= cls+1 && c < arenaPoolNumClasses; c++ {
		stack := p.classes[c]
		for len(stack) > 0 {
			lastIdx := len(stack) - 1
			wp := stack[lastIdx]
			stack = stack[:lastIdx]
			p.classes[c] = stack
			if v := wp.Value(); v != nil {
				v.Key = key
				p.mu.Unlock()
				return v
			}
			// Weak pointer was reclaimed by GC; keep popping.
		}
	}
	p.mu.Unlock()

	return &arena.PoolItem{
		Arena: arena.NewMonotonicArena(arena.WithMinBufferSize(expected)),
		Key:   key,
	}
}

// Release records the request's actual arena usage, resets the arena, and
// returns it to its size class — unless its capacity exceeds the retention
// cap, in which case its memory is released immediately.
func (p *sizedArenaPool) Release(item *arena.PoolItem) {
	used := item.Arena.Len()
	item.Arena.Reset()

	p.mu.Lock()

	if s, ok := p.sizes[item.Key]; ok {
		if s.count == 50 {
			s.count = 1
			s.totalBytes = s.totalBytes / 50
		}
		s.count++
		s.totalBytes += used
	} else {
		p.sizes[item.Key] = &arenaKeyStats{count: 1, totalBytes: used}
	}

	item.Key = 0

	capacity := item.Arena.Cap()
	cls := arenaSizeClass(capacity)
	if capacity > p.maxRetainedArenaBytes && p.pooledInClass(cls) >= maxOversizePooledPerClass {
		p.mu.Unlock()
		// Free the arena's buffers now instead of waiting for the GC to
		// notice the unreferenced item.
		item.Arena.Release()
		return
	}
	p.classes[cls] = append(p.classes[cls], weak.Make(item))
	p.mu.Unlock()
}

// pooledInClass counts alive pooled arenas in a class, compacting entries
// whose weak pointers the GC has reclaimed. Caller must hold p.mu.
func (p *sizedArenaPool) pooledInClass(cls int) int {
	alive := p.classes[cls][:0]
	for _, wp := range p.classes[cls] {
		if wp.Value() != nil {
			alive = append(alive, wp)
		}
	}
	p.classes[cls] = alive
	return len(alive)
}

// expectedSize returns the rolling-average per-request arena usage recorded
// for the given operation key, or 0 when the key has no history.
func (p *sizedArenaPool) expectedSize(key uint64) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.sizes[key]; ok && s.count > 0 {
		return s.totalBytes / s.count
	}
	return 0
}

// retainedBytes reports the capacity currently parked in the pool: the total
// and maximum capacity of pooled arenas still alive, and their count. Used by
// benchmarks and tests to assert retention bounds.
func (p *sizedArenaPool) retainedBytes() (total, maxCap, count int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for c := range p.classes {
		for _, wp := range p.classes[c] {
			v := wp.Value()
			if v == nil {
				continue
			}
			capacity := v.Arena.Cap()
			total += capacity
			count++
			if capacity > maxCap {
				maxCap = capacity
			}
		}
	}
	return total, maxCap, count
}
