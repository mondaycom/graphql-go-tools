package plan

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wundergraph/graphql-go-tools/v2/pkg/astnormalization"
	"github.com/wundergraph/graphql-go-tools/v2/pkg/asttransform"
	"github.com/wundergraph/graphql-go-tools/v2/pkg/astvalidation"
	"github.com/wundergraph/graphql-go-tools/v2/pkg/internal/unsafeparser"
	"github.com/wundergraph/graphql-go-tools/v2/pkg/operationreport"
)

// WithCostConfig attaches a DataSourceCostConfig to a dsBuilder.
func (b *dsBuilder) WithCostConfig(cfg *DataSourceCostConfig) *dsBuilder {
	b.ds.DataSourceMetadata.CostConfig = cfg
	return b
}

// planCostCalc runs the full planner pipeline and returns the CostCalculator.
// Requires ComputeCosts: true in config.
func planCostCalc(t *testing.T, schema, query string, config Configuration) *CostCalculator {
	t.Helper()

	def := unsafeparser.ParseGraphqlDocumentString(schema)
	op := unsafeparser.ParseGraphqlDocumentString(query)
	require.NoError(t, asttransform.MergeDefinitionWithBaseSchema(&def))

	var report operationreport.Report
	astnormalization.NewNormalizer(true, true).NormalizeOperation(&op, &def, &report)
	astvalidation.DefaultOperationValidator().Validate(&op, &def, &report)
	require.False(t, report.HasErrors(), report.Error())

	p, err := NewPlanner(config)
	require.NoError(t, err)

	result := p.Plan(&op, &def, "", &report)
	require.False(t, report.HasErrors(), report.Error())

	sync, ok := result.(*SynchronousResponsePlan)
	require.True(t, ok)
	require.NotNil(t, sync.CostCalculator)
	return sync.CostCalculator
}

// findNode returns the first cost tree node with the given field coordinates, or nil.
func findNode(root *CostTreeNode, coord FieldCoordinate) *CostTreeNode {
	if root == nil {
		return nil
	}
	if root.fieldCoords == coord {
		return root
	}
	for _, child := range root.children {
		if n := findNode(child, coord); n != nil {
			return n
		}
	}
	return nil
}

// TestCostVisitor_ListOfScalarClassification verifies that the cost visitor correctly
// classifies list-of-scalar fields (e.g. [String!]!) as simple types (returnsSimpleType=true).
//
// This is the root cause of the Apollo (27) vs Cosmo (114) discrepancy for:
//
//	query { user_configs { kind role_id visibility behaviors } }
//
// TypeIsScalar/TypeIsEnum stop recursing at TypeKindList, so without the fix [String!]!
// returns isSimpleType=false and falls through to ObjectTypeWeight("String")=1.
func TestCostVisitor_ListOfScalarClassification(t *testing.T) {
	const schema = `
		type Query {
			user_configs: [UserConfig!]!
		}
		type UserConfig {
			kind:       String!
			role_id:    String!
			visibility: [String!]!
			behaviors:  [String!]!
		}
	`
	const query = `query { user_configs { kind role_id visibility behaviors } }`

	ds := dsb().
		Schema(schema).
		RootNode("Query", "user_configs").
		ChildNode("UserConfig", "kind", "role_id", "visibility", "behaviors").
		WithCostConfig(&DataSourceCostConfig{}).
		DS()

	tree := planCostCalc(t, schema, query, Configuration{
		ComputeCosts:              true,
		StaticCostDefaultListSize: 1,
		DataSources:               []DataSource{ds},
	}).tree

	tests := []struct {
		coord      FieldCoordinate
		wantSimple bool
		wantList   bool
	}{
		// String! — plain scalar, always simple
		{FieldCoordinate{"UserConfig", "kind"}, true, false},
		// [String!]! — list-of-scalar: simple after the fix, object without it
		{FieldCoordinate{"UserConfig", "visibility"}, true, true},
		{FieldCoordinate{"UserConfig", "behaviors"}, true, true},
		// [UserConfig!]! — list of object, never simple
		{FieldCoordinate{"Query", "user_configs"}, false, true},
	}

	for _, tc := range tests {
		node := findNode(tree, tc.coord)
		require.NotNil(t, node, "node not found: %v", tc.coord)
		assert.Equal(t, tc.wantSimple, node.returnsSimpleType, "%v.returnsSimpleType", tc.coord)
		assert.Equal(t, tc.wantList, node.returnsListType, "%v.returnsListType", tc.coord)
	}
}
