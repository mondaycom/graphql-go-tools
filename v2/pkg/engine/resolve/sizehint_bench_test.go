package resolve

import (
	"bytes"
	"math/rand"
	"testing"
)

// These benchmarks quantify the effect of the subgraph fetch buffer size hint
// headroom (see GetOrCreateItem). The fetch path allocates a bytes.Buffer with
// cap = sizeHint and fills it via ReadFrom; whenever the actual response
// exceeds the hint the buffer doubles and copies. Response sizes fluctuate
// around their rolling mean, so hinting exactly the mean forces a growth copy
// on roughly half of all fetches.

// sizeHintResponseSizes models per-fetch response sizes fluctuating ±50%
// around a 64KB mean, deterministic across runs.
func sizeHintResponseSizes(n int) []int {
	r := rand.New(rand.NewSource(7))
	sizes := make([]int, n)
	for i := range sizes {
		sizes[i] = 32<<10 + r.Intn(64<<10) // 32KB..96KB, mean 64KB
	}
	return sizes
}

func benchmarkSizeHint(b *testing.B, headroom func(mean int) int) {
	sizes := sizeHintResponseSizes(1024)
	payload := make([]byte, 128<<10)

	// Rolling mean tracked the same way Finish does.
	count, totalBytes := 0, 0

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		size := sizes[i%len(sizes)]
		hint := 64
		if count > 0 {
			hint = headroom(totalBytes / count)
		}
		buf := bytes.NewBuffer(make([]byte, 0, hint))
		_, _ = buf.ReadFrom(bytes.NewReader(payload[:size]))
		if count == 50 {
			count = 1
			totalBytes = totalBytes / 50
		}
		count++
		totalBytes += size
	}
}

func Benchmark_FetchBufferSizeHint_Mean(b *testing.B) {
	benchmarkSizeHint(b, func(mean int) int { return mean })
}

func Benchmark_FetchBufferSizeHint_MeanPlus25(b *testing.B) {
	benchmarkSizeHint(b, func(mean int) int { return mean + mean/4 })
}
