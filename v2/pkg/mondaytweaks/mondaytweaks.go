// Package mondaytweaks defines compile-time feature flags for monday.com-specific
// behavioural overrides. Both the astnormalization and engine packages import this
// package so all monday-specific toggles live in one place.
package mondaytweaks

const (
	// UseInterfaceDefaultCostForAbstractTypes makes the cost calculator use a field's
	// return-type default weight (scalar=0, object=1) for abstract-type selections instead
	// of the maximum @cost weight across all implementing types — matches Apollo Router.
	UseInterfaceDefaultCostForAbstractTypes = true

	// CoerceNullVariablesWithDefaults enables the null variable coercion normalizer.
	// When a nullable variable is explicitly null and used in a non-null argument position
	// that has a schema default, the variable reference is split so the subgraph treats it
	// as "not provided" and applies the schema default — matching Apollo Router behavior.
	CoerceNullVariablesWithDefaults = true

	// FixListOfScalarCostClassification makes the cost visitor correctly classify list-of-scalar
	// fields (e.g. [String!]!) as simple types (weight 0) during cost-tree construction.
	// Without this fix, TypeIsScalar/TypeIsEnum stop recursing at the list boundary, so
	// [String!]! falls through to ObjectTypeWeight("String")=1 and every item in the list
	// is charged as if it were an object — causing Cosmo to over-count vs Apollo.
	FixListOfScalarCostClassification = true

	// SkipEntityResolutionPlannerCostForParentField prevents entity-resolution planners from
	// inflating the cost of the parent list field they traverse through. When a field (e.g.
	// Team.name) is owned by a different subgraph and requires an _entities call, the entity
	// resolution planner registers itself as a visitor of the parent list field (e.g.
	// Query.teams) so it can walk into the selection set. Without this fix, the cost visitor
	// counts that planner as a second data source for Query.teams and charges
	// ObjectTypeWeight("Team")=1 per item on top of the primary subgraph's configured weight —
	// violating the IBM Cost Specification, which bases costs on the user's operation, not the
	// router's internal fetch strategy.
	//
	// With this fix, getFieldDataSourceHashes skips any planner that does not own the field
	// via a PathTypeField entry (HasPathWithFieldRef), i.e. planners that merely traverse
	// through the field to reach a child.
	SkipEntityResolutionPlannerCostForParentField = true
)
