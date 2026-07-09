package resolve

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	arena "github.com/wundergraph/go-arena"
)

const responseBufferTestChunk = 512

// writeInChunks simulates the resolver rendering a response through many
// small writes (one per scalar/field), totalling `total` bytes.
func writeInChunks(t testing.TB, w interface {
	Write(p []byte) (int, error)
}, total int) {
	chunk := make([]byte, responseBufferTestChunk)
	for written := 0; written < total; written += len(chunk) {
		n, err := w.Write(chunk)
		require.NoError(t, err)
		require.Equal(t, len(chunk), n)
	}
}

func TestResponseBuffer_Correctness(t *testing.T) {
	a := arena.NewMonotonicArena()
	buf := newResponseBuffer(a, 0)

	payload := []byte(`{"data":{"hello":"world"}}`)
	for _, b := range payload {
		_, err := buf.Write([]byte{b})
		require.NoError(t, err)
	}
	assert.Equal(t, payload, buf.Bytes())
	assert.Equal(t, len(payload), buf.Len())
}

// TestResponseBuffer_ArenaConsumption documents why responseBuffer replaces
// arena.Buffer on the response path: arena.Buffer's 1.25× growth abandons
// every previous backing region inside the monotonic arena, so an R-byte
// response consumes several times R of arena space. responseBuffer with a
// size hint consumes ~R.
func TestResponseBuffer_ArenaConsumption(t *testing.T) {
	const responseSize = 1 << 20

	arenaBufferArena := arena.NewMonotonicArena()
	arenaBuffer := arena.NewArenaBuffer(arenaBufferArena)
	writeInChunks(t, arenaBuffer, responseSize)
	arenaBufferConsumed := arenaBufferArena.Len()

	hintedArena := arena.NewMonotonicArena()
	hinted := newResponseBuffer(hintedArena, responseSize+responseSize/4)
	writeInChunks(t, hinted, responseSize)
	hintedConsumed := hintedArena.Len()

	unhintedArena := arena.NewMonotonicArena()
	unhinted := newResponseBuffer(unhintedArena, 0)
	writeInChunks(t, unhinted, responseSize)
	unhintedConsumed := unhintedArena.Len()

	// arena.Buffer: geometric 1.25× growth leaves ~4-5× the response size.
	assert.Greater(t, arenaBufferConsumed, 3*responseSize)
	// hinted responseBuffer: a single allocation, ~1.25× the response size.
	assert.Less(t, hintedConsumed, responseSize+responseSize/2)
	// unhinted responseBuffer: doubling leaves at most ~2× + final capacity.
	assert.Less(t, unhintedConsumed, 3*responseSize)

	t.Logf("arena.Buffer=%d hinted=%d unhinted=%d (response=%d)",
		arenaBufferConsumed, hintedConsumed, unhintedConsumed, responseSize)
}

func Benchmark_ResponseBuffer(b *testing.B) {
	const responseSize = 1 << 20
	chunk := make([]byte, responseBufferTestChunk)

	b.Run("arena.Buffer", func(b *testing.B) {
		a := arena.NewMonotonicArena()
		b.ReportAllocs()
		for b.Loop() {
			buf := arena.NewArenaBuffer(a)
			for written := 0; written < responseSize; written += len(chunk) {
				_, _ = buf.Write(chunk)
			}
			b.ReportMetric(float64(a.Len())/(1<<20), "arena-MB")
			a.Reset()
		}
	})

	b.Run("responseBuffer-hinted", func(b *testing.B) {
		a := arena.NewMonotonicArena()
		b.ReportAllocs()
		for b.Loop() {
			buf := newResponseBuffer(a, responseSize+responseSize/4)
			for written := 0; written < responseSize; written += len(chunk) {
				_, _ = buf.Write(chunk)
			}
			b.ReportMetric(float64(a.Len())/(1<<20), "arena-MB")
			a.Reset()
		}
	})
}
