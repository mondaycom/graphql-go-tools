package postprocess

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOrderSequenceByDependencies_ProcessFetchTree(t *testing.T) {
	t.Run("no dependencies", func(t *testing.T) {
		processor := &orderSequenceByDependencies{}
		input := seq(
			sf(2),
			sf(0),
			sf(1),
		)
		processor.ProcessFetchTree(input)
		expected := seq(
			sf(0),
			sf(1),
			sf(2),
		)
		require.Equal(t, expected, input)
	})
	t.Run("serial dependencies", func(t *testing.T) {
		processor := &orderSequenceByDependencies{}
		input := seq(
			sf(0),
			sf(2, dependsOn(1)),
			sf(1, dependsOn(0)),
		)
		processor.ProcessFetchTree(input)
		expected := seq(
			sf(0),
			sf(1, dependsOn(0)),
			sf(2, dependsOn(1)),
		)
		require.Equal(t, expected, input)
	})
	t.Run("serial + requires dependencies", func(t *testing.T) {
		processor := &orderSequenceByDependencies{}
		input := seq(
			sf(0),
			sf(1, dependsOn(0, 2)),
			sf(2, dependsOn(0)),
		)
		processor.ProcessFetchTree(input)
		expected := seq(
			sf(0),
			sf(2, dependsOn(0)),
			sf(1, dependsOn(0, 2)),
		)
		require.Equal(t, expected, input)
	})
	t.Run("more dependencies", func(t *testing.T) {
		processor := &orderSequenceByDependencies{}
		input := seq(
			sf(4, dependsOn(3)),
			sf(0),
			sf(2, dependsOn(1)),
			sf(3, dependsOn(5, 1)),
			sf(1, dependsOn(0)),
			sf(5, dependsOn(0)),
		)
		processor.ProcessFetchTree(input)
		expected := seq(
			sf(0),
			sf(1, dependsOn(0)),
			sf(5, dependsOn(0)),
			sf(2, dependsOn(1)),
			sf(3, dependsOn(5, 1)),
			sf(4, dependsOn(3)),
		)
		require.Equal(t, expected, input)
	})
	t.Run("double dependencies", func(t *testing.T) {
		processor := &orderSequenceByDependencies{}
		input := seq(
			sf(0),
			sf(1, dependsOn(0)),
			sf(2, dependsOn(0, 5)),
			sf(3, dependsOn(0, 1)),
			sf(4, dependsOn(2)),
			sf(5, dependsOn(0)),
		)
		processor.ProcessFetchTree(input)
		expected := seq(
			sf(0),
			sf(1, dependsOn(0)),
			sf(5, dependsOn(0)),
			sf(2, dependsOn(0, 5)),
			sf(3, dependsOn(0, 1)),
			sf(4, dependsOn(2)),
		)
		require.Equal(t, expected, input)
	})
	t.Run("double dependencies variant", func(t *testing.T) {
		processor := &orderSequenceByDependencies{}
		input := seq(
			sf(0),
			sf(2, dependsOn(0, 1)),
			sf(1, dependsOn(0)),
			sf(3, dependsOn(2)),
			sf(5, dependsOn(4)),
			sf(4, dependsOn(2, 3)),
		)
		processor.ProcessFetchTree(input)
		expected := seq(
			sf(0),
			sf(1, dependsOn(0)),
			sf(2, dependsOn(0, 1)),
			sf(3, dependsOn(2)),
			sf(4, dependsOn(2, 3)),
			sf(5, dependsOn(4)),
		)
		require.Equal(t, expected, input)
	})
	t.Run("nested requires", func(t *testing.T) {
		processor := &orderSequenceByDependencies{}
		input := seq(
			sf(0),
			sf(3, dependsOn(0, 2)),
			sf(1, dependsOn(0)),
			sf(2, dependsOn(0)),
			sf(4, dependsOn(0, 1)),
		)
		processor.ProcessFetchTree(input)
		expected := seq(
			sf(0),
			sf(1, dependsOn(0)),
			sf(2, dependsOn(0)),
			sf(3, dependsOn(0, 2)),
			sf(4, dependsOn(0, 1)),
		)
		require.Equal(t, expected, input)
	})

	t.Run("dependent with fetch ID 0 must come after its dependency", func(t *testing.T) {
		processor := &orderSequenceByDependencies{}
		input := seq(
			sf(0, dependsOn(3)),
			sf(3, dependsOn(1, 2)),
			sf(1, dependsOn(5)),
			sf(2, dependsOn(5)),
			sf(5),
		)
		processor.ProcessFetchTree(input)
		expected := seq(
			sf(5),
			sf(1, dependsOn(5)),
			sf(2, dependsOn(5)),
			sf(3, dependsOn(1, 2)),
			sf(0, dependsOn(3)),
		)
		require.Equal(t, expected, input)
	})
	t.Run("equal transitive dependencies tie-break by fetch ID (diamond)", func(t *testing.T) {
		processor := &orderSequenceByDependencies{}
		input := seq(
			sf(7, dependsOn(4, 5)),
			sf(6, dependsOn(3, 4, 5)),
			sf(3),
			sf(4, dependsOn(3)),
			sf(5, dependsOn(3)),
		)
		processor.ProcessFetchTree(input)
		expected := seq(
			sf(3),
			sf(4, dependsOn(3)),
			sf(5, dependsOn(3)),
			sf(6, dependsOn(3, 4, 5)),
			sf(7, dependsOn(4, 5)),
		)
		require.Equal(t, expected, input)
	})
	t.Run("duplicate direct dependency IDs tie-break by fetch ID", func(t *testing.T) {
		processor := &orderSequenceByDependencies{}
		input := seq(
			sf(3, dependsOn(1)),
			sf(2, dependsOn(1, 1)),
			sf(1),
		)
		processor.ProcessFetchTree(input)
		expected := seq(
			sf(1),
			sf(2, dependsOn(1, 1)),
			sf(3, dependsOn(1)),
		)
		require.Equal(t, expected, input)
	})
	// Regression for the O(2^N) blowup: a densely-connected fetch tree where node
	// i depends on every earlier node [0..i-1] — the shape produced by mutations
	// with many aliased root fields (e.g. 28-31 aliased delete_webhook). The old
	// unmemoized recursive nodeDependsOn re-derived each node's transitive set on
	// every comparison, so a tree this size would never finish. With memoization
	// the result is computed once per ID and the test returns effectively instantly.
	// Reaching the assertion at all proves the exponential is gone; we also assert
	// the ordering is the expected ascending-by-fetchID sequence (0..N-1).
	t.Run("dense fully-connected chain (exponential regression)", func(t *testing.T) {
		const n = 31
		// Shuffle the input order so the sort has real work to do rather than
		// receiving an already-sorted slice.
		input := make([]resolve.FetchDependencies, 0, n)
		for i := n - 1; i >= 0; i-- {
			dependsOn := make([]int, 0, i)
			for j := 0; j < i; j++ {
				dependsOn = append(dependsOn, j)
			}
			input = append(input, resolve.FetchDependencies{FetchID: i, DependsOnFetchIDs: dependsOn})
		}
		seq := depsToSequence(input)
		processor.ProcessFetchTree(seq)
		got := sequenceToDeps(seq)
		require.Len(t, got, n)
		for i := range n {
			require.Equal(t, i, got[i].FetchID, "node at position %d should be fetchID %d", i, i)
		}
	})
}
