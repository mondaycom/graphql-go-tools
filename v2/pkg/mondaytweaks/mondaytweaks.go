// Package mondaytweaks defines compile-time feature flags for monday.com-specific
// behavioural overrides. Both the astnormalization and engine packages import this
// package so all monday-specific toggles live in one place.
package mondaytweaks

const (
	// CoerceNullVariablesWithDefaults enables the null variable coercion normalizer.
	// When a nullable variable is explicitly null and used in a non-null argument position
	// that has a schema default, the variable reference is split so the subgraph treats it
	// as "not provided" and applies the schema default — matching Apollo Router behavior.
	CoerceNullVariablesWithDefaults = true

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

	// CloseWSConnectionsOnContextCancel makes the WSTransport forcibly close all active
	// WebSocket connections when its parent context is cancelled. Without this, the pingLoop
	// exits on context cancellation but individual connections — whose readLoop blocks on
	// protocol.Read(context.Background()) — stay alive indefinitely, pinning the entire
	// object chain (WSTransport → SubscriptionClient → Factory → DataSources → PlanConfig →
	// Executor → RouterSchema *ast.Document ~200MB) until the remote end closes the socket.
	CloseWSConnectionsOnContextCancel = true

	// MemoizeFetchDependencyOrdering switches orderSequenceByDependencies.ProcessFetchTree
	// to a memoized fetch-ordering algorithm. The upstream implementation sorts fetch-tree
	// nodes with slices.SortFunc and calls nodeDependsOn twice per comparison; nodeDependsOn
	// recurses with no memoization and looks up nodes via an O(N) linear scan of
	// root.ChildNodes. For densely-connected fetch trees — the aliased-mutation shape where
	// fetch i depends on [0..i-1] — this is O(2^N) and dominates planning CPU (prod
	// ap-southeast-2 saw 28-31 aliased delete_webhook mutations at 200-993ms of pure
	// planning each).
	//
	// With this fix, ProcessFetchTree precomputes once per call a fetchID->node index and a
	// memoized transitive-dependency map (memoized DFS, in-progress set guards cycles); the
	// comparator reads the precomputed sets. The comparator logic is byte-identical to the
	// upstream path, so output ordering is unchanged. When this flag is false the original
	// recursive path runs unchanged.
	MemoizeFetchDependencyOrdering = true

	// ApolloRouterCompatibilitySubrequestHTTPError makes the Loader attach the SUBREQUEST_HTTP_ERROR
	// code to non-2XX responses with no GraphQL errors body. This is a compatibility mode for Apollo Router.
	ApolloRouterCompatibilitySubrequestHTTPError = true
)

var (
	// MergeContiguousMutationRootFields allows contiguous mutation root fields planned on
	// the same subgraph to share one upstream mutation fetch while preserving alias order.
	// It deliberately only merges adjacent same-subgraph runs so GraphQL's serial mutation
	// semantics are preserved across subgraph boundaries.
	MergeContiguousMutationRootFields = true

	// OptimizeVariablesExtraction linearizes the variable-extraction normalizer, whose
	// upstream implementation is super-linear on the aliased-batch mutation shape (one
	// anonymous mutation with N aliased root fields, all arguments inline). For such
	// operations extraction cost is dominated by two super-linear hotspots:
	//
	//   1. Document.GenerateUnusedVariableDefinitionName restarts its name search from "a"
	//      on every call and linearly scans the operation's variable definitions for each
	//      candidate — O(N^3) over the batch. This dominates.
	//   2. variableExists deduplicates each inline value by walking the entire (growing)
	//      Input.Variables JSON and linearly scanning the extracted-variable list — O(N^2).
	//
	// Prod (US cluster group 02) saw single anonymous mutations spend 200-570ms in
	// normalization purely on this shape, inflating tail latency and GC pressure.
	//
	// With this fix the visitor (single-operation documents only) generates names from a
	// monotonic cursor that skips only pre-existing user variable names (O(1) amortized) and
	// deduplicates via a (canonical-type, value) map (O(1) per argument), replacing the two
	// dominant super-linear hotspots. The remaining per-variable sjson.SetRawBytes write is
	// addressed separately by BatchExtractedVariablesJSON. Output is byte-identical to the
	// upstream path; the differential test in variables_extraction_optimized_test.go asserts
	// this across the extraction corpus plus batch shapes. Multi-operation documents fall back
	// to the original path, where generated names are shared across operations through the
	// shared Input.Variables buffer. When this flag is false the original path runs unchanged.
	OptimizeVariablesExtraction = true

	// BatchExtractedVariablesJSON removes the per-variable sjson.SetRawBytes write in the
	// variable-extraction normalizer. Each sjson.SetRawBytes call re-parses and re-serialises
	// the entire (growing) Input.Variables buffer, so extracting N inline arguments copies
	// O(N^2) bytes; on the aliased-batch mutation shape sjson.appendRawPaths was the single
	// largest transient allocator in the extraction profile (~40% of allocations). This flag
	// switches the optimized path to append each new "name":value member in place into a
	// per-document owned buffer (amortised O(1) per write, O(N) total), keeping Input.Variables
	// a valid JSON object after every write so uploads.FindUploads — which parses it on each
	// argument — still observes the same bytes it does today. Generated names never pre-exist
	// in the buffer, so they append at the end in first-occurrence order, exactly where sjson
	// places them: output is byte-identical to the sjson path (asserted by the differential
	// test with this flag toggled). Only takes effect when OptimizeVariablesExtraction is also
	// enabled (single-operation documents); when false the per-variable sjson write runs
	// unchanged.
	//
	// Byte-order note: sjson.SetRawBytes prepends each new top-level key, so the sjson path
	// emits extracted variables in reverse first-occurrence order, whereas the in-place append
	// emits them in forward order. The two Input.Variables buffers are therefore
	// key-reordered, not byte-identical. This is functionally invisible — the operation AST
	// (which references $a, $b, ...) is identical on both paths, RemapVariables canonicalises
	// variable order before the operation hash, and the variables object is order-independent
	// at execution — but it means the byte-exact upstream extraction corpus only matches the
	// sjson path. This flag therefore defaults OFF (upstream tests keep the sjson bytes) and is
	// intended to be enabled together with OptimizeVariablesExtraction as a per-cluster canary;
	// TestVariablesExtraction_BatchedMatchesSemantically asserts the two paths are semantically
	// equal (identical AST, deep-equal variables) across the corpus and batch shapes.
	BatchExtractedVariablesJSON = false
)
