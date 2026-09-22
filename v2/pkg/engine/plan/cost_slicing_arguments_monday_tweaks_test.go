package plan

// Tests for list-valued @listSize slicingArguments.
// See mondaytweaks.ResolveArraySlicingArguments for the corresponding feature flag.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wundergraph/astjson"

	"github.com/wundergraph/graphql-go-tools/v2/pkg/engine/resolve"
	"github.com/wundergraph/graphql-go-tools/v2/pkg/mondaytweaks"
)

func varsFromJSON(t *testing.T, input string) resolve.VariablesView {
	t.Helper()
	value, err := astjson.Parse(input)
	require.NoError(t, err)
	return resolve.NewVariablesView(value, nil)
}

// simpleArgs builds ArgumentInfo entries that point every argument name to a
// variable of the same name, which is how the engine represents arguments after
// inline literals have been converted to variables.
func simpleArgs(names ...string) map[string]ArgumentInfo {
	args := make(map[string]ArgumentInfo, len(names))
	for _, name := range names {
		args[name] = ArgumentInfo{hasVariable: true, varName: name, isSimple: true}
	}
	return args
}

func withArraySlicingArguments(t *testing.T, enabled bool) {
	t.Helper()
	previous := mondaytweaks.ResolveArraySlicingArguments.Load()
	mondaytweaks.ResolveArraySlicingArguments.Store(enabled)
	t.Cleanup(func() {
		mondaytweaks.ResolveArraySlicingArguments.Store(previous)
	})
}

func TestListSizeMultiplierWithListSlicingArgument(t *testing.T) {
	const defaultListSize = 50

	t.Run("list argument resolves to its length", func(t *testing.T) {
		withArraySlicingArguments(t, true)
		ls := &FieldListSize{SlicingArguments: []string{"ids"}, AssumedSize: 100}
		vars := varsFromJSON(t, `{"ids":["1","2","3"]}`)
		assert.Equal(t, 3, ls.multiplier(simpleArgs("ids"), vars, defaultListSize))
	})

	t.Run("non-empty list wins over a larger Int argument", func(t *testing.T) {
		withArraySlicingArguments(t, true)
		ls := &FieldListSize{SlicingArguments: []string{"ids", "limit"}}
		vars := varsFromJSON(t, `{"ids":["1","2","3"],"limit":100}`)
		assert.Equal(t, 3, ls.multiplier(simpleArgs("ids", "limit"), vars, defaultListSize))
	})

	t.Run("non-empty list wins over a smaller Int argument", func(t *testing.T) {
		withArraySlicingArguments(t, true)
		ls := &FieldListSize{SlicingArguments: []string{"ids", "limit"}}
		vars := varsFromJSON(t, `{"ids":["1","2","3"],"limit":2}`)
		assert.Equal(t, 3, ls.multiplier(simpleArgs("ids", "limit"), vars, defaultListSize))
	})

	t.Run("largest list wins among several lists", func(t *testing.T) {
		withArraySlicingArguments(t, true)
		ls := &FieldListSize{SlicingArguments: []string{"ids", "names"}}
		vars := varsFromJSON(t, `{"ids":["1","2"],"names":["a","b","c","d"]}`)
		assert.Equal(t, 4, ls.multiplier(simpleArgs("ids", "names"), vars, defaultListSize))
	})

	t.Run("empty list does not win over an Int argument", func(t *testing.T) {
		withArraySlicingArguments(t, true)
		ls := &FieldListSize{SlicingArguments: []string{"ids", "limit"}}
		vars := varsFromJSON(t, `{"ids":[],"limit":25}`)
		assert.Equal(t, 25, ls.multiplier(simpleArgs("ids", "limit"), vars, defaultListSize))
	})

	t.Run("empty list falls back to assumedSize", func(t *testing.T) {
		withArraySlicingArguments(t, true)
		ls := &FieldListSize{SlicingArguments: []string{"ids"}, AssumedSize: 10}
		vars := varsFromJSON(t, `{"ids":[]}`)
		assert.Equal(t, 10, ls.multiplier(simpleArgs("ids"), vars, defaultListSize))
	})

	t.Run("empty list falls back to the default list size", func(t *testing.T) {
		withArraySlicingArguments(t, true)
		ls := &FieldListSize{SlicingArguments: []string{"ids"}}
		vars := varsFromJSON(t, `{"ids":[]}`)
		assert.Equal(t, defaultListSize, ls.multiplier(simpleArgs("ids"), vars, defaultListSize))
	})

	t.Run("null list falls back to the default list size", func(t *testing.T) {
		withArraySlicingArguments(t, true)
		ls := &FieldListSize{SlicingArguments: []string{"ids"}}
		vars := varsFromJSON(t, `{"ids":null}`)
		assert.Equal(t, defaultListSize, ls.multiplier(simpleArgs("ids"), vars, defaultListSize))
	})

	t.Run("nested list argument resolves to its length", func(t *testing.T) {
		withArraySlicingArguments(t, true)
		ls := &FieldListSize{SlicingArguments: []string{"input.ids"}}
		vars := varsFromJSON(t, `{"input":{"ids":["1","2","3","4","5"]}}`)
		args := map[string]ArgumentInfo{
			"input": {hasVariable: true, varName: "input", isInputObject: true},
		}
		assert.Equal(t, 5, ls.multiplier(args, vars, defaultListSize))
	})

	t.Run("nested list wins over a nested Int argument", func(t *testing.T) {
		withArraySlicingArguments(t, true)
		ls := &FieldListSize{SlicingArguments: []string{"input.ids", "input.limit"}}
		vars := varsFromJSON(t, `{"input":{"ids":["1","2"],"limit":100}}`)
		args := map[string]ArgumentInfo{
			"input": {hasVariable: true, varName: "input", isInputObject: true},
		}
		assert.Equal(t, 2, ls.multiplier(args, vars, defaultListSize))
	})

	t.Run("list argument overrides an Int schema default of another argument", func(t *testing.T) {
		withArraySlicingArguments(t, true)
		ls := &FieldListSize{
			SlicingArguments:        []string{"ids", "limit"},
			SlicingArgumentDefaults: map[string]int{"limit": 25},
		}
		vars := varsFromJSON(t, `{"ids":["1","2","3"]}`)
		assert.Equal(t, 3, ls.multiplier(simpleArgs("ids"), vars, defaultListSize))
	})

	t.Run("flag disabled ignores the list argument", func(t *testing.T) {
		withArraySlicingArguments(t, false)
		ls := &FieldListSize{SlicingArguments: []string{"ids", "limit"}}
		vars := varsFromJSON(t, `{"ids":["1","2","3"],"limit":100}`)
		assert.Equal(t, 100, ls.multiplier(simpleArgs("ids", "limit"), vars, defaultListSize))
	})

	t.Run("flag disabled falls back to assumedSize for a list-only argument", func(t *testing.T) {
		withArraySlicingArguments(t, false)
		ls := &FieldListSize{SlicingArguments: []string{"ids"}, AssumedSize: 10}
		vars := varsFromJSON(t, `{"ids":["1","2","3"]}`)
		assert.Equal(t, 10, ls.multiplier(simpleArgs("ids"), vars, defaultListSize))
	})
}

func TestResolveSlicingArgMondayTweak(t *testing.T) {
	t.Run("list counts as provided", func(t *testing.T) {
		ls := &FieldListSize{SlicingArguments: []string{"ids"}}
		vars := varsFromJSON(t, `{"ids":["1","2"]}`)
		size, found, fromList := ls.resolveSlicingArgMondayTweak("ids", simpleArgs("ids"), vars)
		assert.Equal(t, 2, size)
		assert.True(t, found)
		assert.True(t, fromList)
	})

	t.Run("empty list counts as provided", func(t *testing.T) {
		ls := &FieldListSize{SlicingArguments: []string{"ids"}}
		vars := varsFromJSON(t, `{"ids":[]}`)
		size, found, fromList := ls.resolveSlicingArgMondayTweak("ids", simpleArgs("ids"), vars)
		assert.Equal(t, 0, size)
		assert.True(t, found)
		assert.True(t, fromList)
	})

	t.Run("Int is not reported as a list value", func(t *testing.T) {
		ls := &FieldListSize{SlicingArguments: []string{"limit"}}
		vars := varsFromJSON(t, `{"limit":7}`)
		size, found, fromList := ls.resolveSlicingArgMondayTweak("limit", simpleArgs("limit"), vars)
		assert.Equal(t, 7, size)
		assert.True(t, found)
		assert.False(t, fromList)
	})

	t.Run("upstream resolveSlicingArg still ignores lists", func(t *testing.T) {
		ls := &FieldListSize{SlicingArguments: []string{"ids"}}
		vars := varsFromJSON(t, `{"ids":["1","2"]}`)
		size, found := ls.resolveSlicingArg("ids", simpleArgs("ids"), vars)
		assert.Equal(t, 0, size)
		assert.False(t, found)
	})
}
