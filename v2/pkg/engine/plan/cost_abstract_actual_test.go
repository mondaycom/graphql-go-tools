package plan

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/wundergraph/graphql-go-tools/v2/pkg/engine/resolve"
)

// TestActualCost_AbstractTypeInlineFragments verifies that the actual cost of a query with
// inline fragments on abstract (interface/union) list types correctly weights each fragment's
// fields by the fraction of items that are actually of that concrete type.
//
// Without this fix, a field like PeopleValue.text (@cost weight 10) would be charged for
// every item in column_values, even items that are not PeopleValue (which return {} in the
// response). The fix uses __typename counts tracked in actualListSizes to scale each
// inline-fragment child's cost by its actual type distribution.
//
// Schema:
//
//	interface ColumnValue { id: ID! }
//	type PeopleValue implements ColumnValue {
//	    id: ID!
//	    text: String @cost(weight: 10)
//	}
//	type StatusValue implements ColumnValue {
//	    id: ID!
//	    label: String @cost(weight: 5)
//	}
//	type Item { column_values: [ColumnValue!]! }
//	type Query { items: [Item!]! }
//
// Query: items { column_values { ... on PeopleValue { text } } }
func TestActualCost_AbstractTypeInlineFragments(t *testing.T) {
	const dsHash DSHash = 1

	costCfg := &DataSourceCostConfig{
		Weights: map[FieldCoordinate]*FieldCost{
			{TypeName: "PeopleValue", FieldName: "text"}:  {HasWeight: true, Weight: 10},
			{TypeName: "StatusValue", FieldName: "label"}: {HasWeight: true, Weight: 5},
		},
		Types: map[string]int{},
	}

	// newCalc builds a cost tree for:
	//   items { column_values { ... on PeopleValue { text } } }
	newCalcSingle := func() *CostCalculator {
		root := &CostTreeNode{fieldCoords: FieldCoordinate{"_none", "_root"}}
		items := &CostTreeNode{
			parent: root, dataSourceHashes: []DSHash{dsHash},
			fieldCoords: FieldCoordinate{"Query", "items"}, returnsListType: true, jsonPath: "items",
		}
		columnValues := &CostTreeNode{
			parent: items, dataSourceHashes: []DSHash{dsHash},
			fieldCoords: FieldCoordinate{"Item", "column_values"},
			returnsListType: true, returnsAbstractType: true,
			// implementingTypeNames drives fieldCost via ObjectTypeWeight; the CostVisitor
			// populates this from the schema in production.
			implementingTypeNames: []string{"PeopleValue", "StatusValue"},
			fieldTypeName:         "ColumnValue",
			jsonPath:              "items.column_values",
		}
		text := &CostTreeNode{
			parent: columnValues, dataSourceHashes: []DSHash{dsHash},
			fieldCoords: FieldCoordinate{"PeopleValue", "text"}, returnsSimpleType: true,
			jsonPath: "items.column_values.text",
		}
		columnValues.children = []*CostTreeNode{text}
		items.children = []*CostTreeNode{columnValues}
		root.children = []*CostTreeNode{items}
		return &CostCalculator{
			tree:            root,
			costConfigs:     map[DSHash]*DataSourceCostConfig{dsHash: costCfg},
			defaultListSize: 1,
		}
	}

	t.Run("0 of 5 items are PeopleValue: text cost is zero", func(t *testing.T) {
		// column_values has 5 items, none are PeopleValue → no "column_values:PeopleValue" key
		cost := newCalcSingle().ActualCost(resolve.VariablesView{}, map[string]int{
			"items":               1,
			"items.column_values": 5,
			// no "items.column_values:PeopleValue" entry → typeCount = 0
		})
		// fieldCost(column_values) = max(ObjectTypeWeight(PeopleValue,StatusValue)) = 1
		// column_values: (fieldCost=1 + scaledTextCost=0) * 5 = 5
		// items: (fieldCost=1 + 5) * 1 = 6
		assert.Equal(t, 6, cost)
	})

	t.Run("2 of 5 items are PeopleValue: text cost scaled to 2/5", func(t *testing.T) {
		cost := newCalcSingle().ActualCost(resolve.VariablesView{}, map[string]int{
			"items":                           1,
			"items.column_values":             5,
			"items.column_values:PeopleValue": 2,
		})
		// text cost per occurrence = 10; scaled = round(10 * 2/5) = 4
		// column_values: (1 + 4) * 5 = 25
		// items: (1 + 25) * 1 = 26
		assert.Equal(t, 26, cost)
	})

	t.Run("5 of 5 items are PeopleValue: text cost unscaled (full cost)", func(t *testing.T) {
		cost := newCalcSingle().ActualCost(resolve.VariablesView{}, map[string]int{
			"items":                           1,
			"items.column_values":             5,
			"items.column_values:PeopleValue": 5,
		})
		// text cost = 10; scale = 5/5 = 1.0 → no reduction
		// column_values: (1 + 10) * 5 = 55
		// items: (1 + 55) * 1 = 56
		assert.Equal(t, 56, cost)
	})

	// newCalcMulti builds a cost tree for:
	//   items { column_values { ... on PeopleValue { text } ... on StatusValue { label } } }
	newCalcMulti := func() *CostCalculator {
		root := &CostTreeNode{fieldCoords: FieldCoordinate{"_none", "_root"}}
		items := &CostTreeNode{
			parent: root, dataSourceHashes: []DSHash{dsHash},
			fieldCoords: FieldCoordinate{"Query", "items"}, returnsListType: true, jsonPath: "items",
		}
		columnValues := &CostTreeNode{
			parent: items, dataSourceHashes: []DSHash{dsHash},
			fieldCoords:           FieldCoordinate{"Item", "column_values"},
			returnsListType:       true, returnsAbstractType: true,
			implementingTypeNames: []string{"PeopleValue", "StatusValue"},
			fieldTypeName:         "ColumnValue",
			jsonPath:              "items.column_values",
		}
		text := &CostTreeNode{
			parent: columnValues, dataSourceHashes: []DSHash{dsHash},
			fieldCoords: FieldCoordinate{"PeopleValue", "text"}, returnsSimpleType: true,
			jsonPath: "items.column_values.text",
		}
		label := &CostTreeNode{
			parent: columnValues, dataSourceHashes: []DSHash{dsHash},
			fieldCoords: FieldCoordinate{"StatusValue", "label"}, returnsSimpleType: true,
			jsonPath: "items.column_values.label",
		}
		columnValues.children = []*CostTreeNode{text, label}
		items.children = []*CostTreeNode{columnValues}
		root.children = []*CostTreeNode{items}
		return &CostCalculator{
			tree:            root,
			costConfigs:     map[DSHash]*DataSourceCostConfig{dsHash: costCfg},
			defaultListSize: 1,
		}
	}

	t.Run("multiple fragments: each scaled independently by its own type count", func(t *testing.T) {
		// 5 total: 3 PeopleValue, 2 StatusValue
		cost := newCalcMulti().ActualCost(resolve.VariablesView{}, map[string]int{
			"items":                           1,
			"items.column_values":             5,
			"items.column_values:PeopleValue": 3,
			"items.column_values:StatusValue": 2,
		})
		// text:  round(10 * 3/5) = 6
		// label: round(5  * 2/5) = 2
		// column_values: (fieldCost=1 + 6 + 2) * 5 = 45
		// items: (1 + 45) * 1 = 46
		assert.Equal(t, 46, cost)
	})

	t.Run("direct interface field is not scaled (TypeName matches parent fieldTypeName)", func(t *testing.T) {
		// Same tree but with an 'id' field directly on ColumnValue (no inline fragment)
		root := &CostTreeNode{fieldCoords: FieldCoordinate{"_none", "_root"}}
		items := &CostTreeNode{
			parent: root, dataSourceHashes: []DSHash{dsHash},
			fieldCoords: FieldCoordinate{"Query", "items"}, returnsListType: true, jsonPath: "items",
		}
		columnValues := &CostTreeNode{
			parent: items, dataSourceHashes: []DSHash{dsHash},
			fieldCoords:           FieldCoordinate{"Item", "column_values"},
			returnsListType:       true, returnsAbstractType: true,
			implementingTypeNames: []string{"PeopleValue", "StatusValue"},
			fieldTypeName:         "ColumnValue",
			jsonPath:              "items.column_values",
		}
		// id is on the ColumnValue interface directly — TypeName == parent fieldTypeName
		id := &CostTreeNode{
			parent:            columnValues,
			dataSourceHashes:  []DSHash{dsHash},
			fieldCoords:       FieldCoordinate{"ColumnValue", "id"},
			returnsSimpleType: true,
			jsonPath:          "items.column_values.id",
		}
		columnValues.children = []*CostTreeNode{id}
		items.children = []*CostTreeNode{columnValues}
		root.children = []*CostTreeNode{items}

		calc := &CostCalculator{
			tree:            root,
			costConfigs:     map[DSHash]*DataSourceCostConfig{dsHash: costCfg},
			defaultListSize: 1,
		}
		cost := calc.ActualCost(resolve.VariablesView{}, map[string]int{
			"items":                            1,
			"items.column_values":              5,
			"items.column_values:PeopleValue":  3,
		})
		// id is a scalar with no @cost → fieldCost = 0 (EnumScalarWeight)
		// No scaling applied because TypeName "ColumnValue" == fieldTypeName "ColumnValue"
		// column_values: (1 + 0) * 5 = 5; items: (1 + 5) * 1 = 6
		assert.Equal(t, 6, cost)
	})
}
