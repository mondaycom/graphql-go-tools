package resolve

import (
	arena "github.com/wundergraph/go-arena"
)

// responseBuffer accumulates the rendered response on a request arena.
//
// It replaces arena.Buffer for response rendering because arena.Buffer grows
// through arena.SliceAppend, which extends capacity by 1.25× per step and —
// the arena being monotonic — abandons every previous backing region until
// Reset. Rendering an R-byte response from small writes that way consumes
// ~5×R of arena space (Σ R×0.8^k), inflating the arena's recorded usage, the
// pool's retained capacity, and the memclr work done by Reset by the same
// factor.
//
// responseBuffer instead pre-allocates the expected response size (from the
// pool's per-key stats) and doubles on growth, so the common case allocates
// exactly once and the worst case wastes at most ~1× the response size.
type responseBuffer struct {
	arena arena.Arena
	buf   []byte
}

const responseBufferMinSize = 4 << 10

func newResponseBuffer(a arena.Arena, sizeHint int) *responseBuffer {
	if sizeHint < responseBufferMinSize {
		sizeHint = responseBufferMinSize
	}
	return &responseBuffer{
		arena: a,
		buf:   arena.AllocateSlice[byte](a, 0, sizeHint),
	}
}

func (b *responseBuffer) Write(p []byte) (int, error) {
	if need := len(b.buf) + len(p); need > cap(b.buf) {
		newCap := max(cap(b.buf)*2, need)
		grown := arena.AllocateSlice[byte](b.arena, len(b.buf), newCap)
		copy(grown, b.buf)
		b.buf = grown
	}
	b.buf = append(b.buf, p...)
	return len(p), nil
}

// Bytes returns the accumulated response. The slice is arena-backed and only
// valid until the arena is reset or released.
func (b *responseBuffer) Bytes() []byte {
	return b.buf
}

// Len returns the number of bytes accumulated.
func (b *responseBuffer) Len() int {
	return len(b.buf)
}
