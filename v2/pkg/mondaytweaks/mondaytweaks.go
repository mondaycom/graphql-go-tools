// Package mondaytweaks defines runtime-configurable feature flags for monday.com-specific
// behavioural overrides. Both the astnormalization and engine packages import this
// package so all monday-specific toggles live in one place.
//
// All flags are backed by sync/atomic so they are safe to read from concurrent
// request-handling goroutines and to write from the ignite provisioning goroutine.
// Use Flag.Store(v) to change a value (e.g. from an ignite module at boot) and
// Flag.Load() in production code paths.
package mondaytweaks

import "sync/atomic"

var (
	// CoerceNullVariablesWithDefaults enables the null variable coercion normalizer.
	// When a nullable variable is explicitly null and used in a non-null argument position
	// that has a schema default, the variable reference is split so the subgraph treats it
	// as "not provided" and applies the schema default — matching Apollo Router behavior.
	CoerceNullVariablesWithDefaults atomic.Bool

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
	SkipEntityResolutionPlannerCostForParentField atomic.Bool

	// CloseWSConnectionsOnContextCancel makes the WSTransport forcibly close all active
	// WebSocket connections when its parent context is cancelled. Without this, the pingLoop
	// exits on context cancellation but individual connections — whose readLoop blocks on
	// protocol.Read(context.Background()) — stay alive indefinitely, pinning the entire
	// object chain (WSTransport → SubscriptionClient → Factory → DataSources → PlanConfig →
	// Executor → RouterSchema *ast.Document ~200MB) until the remote end closes the socket.
	CloseWSConnectionsOnContextCancel atomic.Bool

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
	MemoizeFetchDependencyOrdering atomic.Bool

	// ApolloRouterCompatibilitySubrequestHTTPError makes the Loader attach the SUBREQUEST_HTTP_ERROR
	// code to non-2XX responses with no GraphQL errors body. This is a compatibility mode for Apollo Router.
	ApolloRouterCompatibilitySubrequestHTTPError atomic.Bool

	// EnableSizedArenaPool switches request arenas in the resolve package to the
	// size-classed, retention-bounded pool (resolve.sizedArenaPool). The upstream
	// arena.Pool hands out any pooled arena regardless of size and never shrinks,
	// so under bimodal traffic every pooled arena converges to the largest
	// response it ever served — steady-state retention is concurrency × largest
	// response (~680MB in the retention benchmark; the sized pool holds
	// ~90-120MB, -83%, with unchanged throughput). Flip to false to fall back to
	// the upstream pool in case a workload regresses.
	EnableSizedArenaPool atomic.Bool

	// MergeContiguousMutationRootFields allows contiguous mutation root fields planned on
	// the same subgraph to share one upstream mutation fetch while preserving alias order.
	// It deliberately only merges adjacent same-subgraph runs so GraphQL's serial mutation
	// semantics are preserved across subgraph boundaries.
	MergeContiguousMutationRootFields atomic.Bool

	// OptimizeVariablesExtraction linearizes the variable-extraction normalizer, whose
	// upstream implementation is super-linear on the aliased-batch mutation shape (one
	// anonymous mutation with N aliased root fields, all arguments inline). For such
	// operations extraction cost is dominated by two super-linear hotspots:
	//
	//  1. Document.GenerateUnusedVariableDefinitionName restarts its name search from "a"
	//     on every call and linearly scans the operation's variable definitions for each
	//     candidate — O(N^3) over the batch. This dominates.
	//  2. variableExists deduplicates each inline value by walking the entire (growing)
	//     Input.Variables JSON and linearly scanning the extracted-variable list — O(N^2).
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
	OptimizeVariablesExtraction atomic.Bool

	// BatchExtractedVariablesJSON removes the per-variable sjson.SetRawBytes write in the
	// variable-extraction normalizer. Each sjson.SetRawBytes call re-parses and re-serialises
	// the entire (growing) Input.Variables buffer, so extracting N inline arguments copies
	// O(N^2) bytes; on the aliased-batch mutation shape sjson.appendRawPaths was the single
	// largest transient allocator in the extraction profile (~40% of allocations).
	//
	// With this flag the optimized path buffers each extracted (name,value) pair in
	// first-occurrence order and defers a single Input.Variables build to LeaveDocument
	// (flushBatchedExtractedVariables), replacing N growing re-serialisations with one
	// O(total bytes) pass. Deferral is safe: no inline value can reference a just-generated
	// variable name, so uploads.FindUploads — the only reader of Input.Variables on this path
	// — needs only the original client variables, which stay untouched during the walk.
	//
	// The build is byte-identical to the sjson path, not merely semantically equal. sjson
	// inserts each not-yet-present top-level key at the FRONT of the object, so after N
	// sequential inserts the extracted variables appear in reverse creation order ahead of any
	// pre-existing client variables; the deferred build emits exactly that order. The
	// differential test in variables_extraction_optimized_test.go asserts byte-identity across
	// the extraction corpus and batch shapes with this flag toggled, and the upstream
	// byte-exact corpus (TestVariablesExtraction, TestInputCoercionForList, TestNormalizeOperation)
	// passes with it enabled. Only takes effect when OptimizeVariablesExtraction is also enabled
	// (single-operation documents); when false the per-variable sjson write runs unchanged.
	BatchExtractedVariablesJSON atomic.Bool

	// CacheRenderFieldPath replaces pool.BytesBuffer in renderFieldPath() with a reusable
	// per-Resolvable scratch []byte (r.fieldPathBuf), and uses the Go compiler's zero-alloc
	// m[string(b)] map-lookup optimisation in recordFieldReached so that looking up an
	// already-seen path costs zero allocations.
	//
	// renderFieldPath() is on the hot path of every cost-control-enabled resolution:
	//   - recordObjectTypeStats  — called per object in the response tree
	//   - walkArray cost block   — called per array field
	//   - recordFieldReached     — called once per unique *Field (deduplicated by pointer)
	//   - renderFieldValue       — called when a custom fieldRenderer is active
	//   - two error paths        — addRejectFieldError and the non-nullable null handler
	//
	// The original implementation calls pool.BytesBuffer.Get() + defer Put() + buf.String()
	// on every call, producing one heap-allocated string and two sync.Pool operations per
	// invocation.  Under cost-control load this was observed at ~3.4 GB alloc / 90 s
	// (~1% of total allocations).
	//
	// With this flag:
	//   - The pool.BytesBuffer Get/Put round-trip is eliminated; r.fieldPathBuf is reset
	//     and reused across calls, growing once to the longest path seen.
	//   - In recordFieldReached the typeNameStats lookup uses string(r.fieldPathBuf) directly
	//     inside the map-index expression; the Go compiler elides the heap allocation for that
	//     read.  Only the INSERT branch (new distinct path) materialises a real string, so the
	//     per-call alloc drops to zero for already-seen paths.
	//   - recordObjectTypeStats and walkArray still need one alloc for the map write-back key,
	//     but avoid the pool overhead.
	//
	// When false, renderFieldPath() and all call sites run exactly as before.
	CacheRenderFieldPath atomic.Bool

	// DisableUploadFinding skips the per-argument upload-discovery pass in the variable-
	// extraction normalizer (uploads.FindUploads). Whenever the schema declares an Upload
	// scalar — which the federated router schema does — that pass calls astjson.ParseBytes on
	// Input.Variables once per argument. On the aliased-batch mutation shape (one anonymous
	// mutation with N aliased root fields) that was originally O(N^2): the pre-batch path grew
	// Input.Variables with each sjson write, so every re-parse saw a larger buffer.
	// BatchExtractedVariablesJSON keeps the buffer constant during the walk, reducing it to N
	// redundant parses (~8 allocations each, O(N*|variables|)); it is still a second per-
	// argument allocator on the same shape.
	//
	// monday never processes multipart file uploads at the router: the gateway resolves
	// uploads before the request reaches the router, so no router request carries an Upload,
	// the discovered upload-path mapping is always empty, and the pass is pure overhead. With
	// this flag the FindUploads call is skipped entirely — the mapping stays empty, exactly the
	// result it would compute — removing all N parses (benchmark: N=200 sheds ~1,600 allocs).
	// When false the original per-argument pass runs unchanged; tests that exercise upload
	// discovery flip it off to assert the upstream behavior.
	DisableUploadFinding atomic.Bool
)

func init() {
	CoerceNullVariablesWithDefaults.Store(true)
	SkipEntityResolutionPlannerCostForParentField.Store(true)
	CloseWSConnectionsOnContextCancel.Store(true)
	MemoizeFetchDependencyOrdering.Store(true)
	ApolloRouterCompatibilitySubrequestHTTPError.Store(true)
	EnableSizedArenaPool.Store(true)
	MergeContiguousMutationRootFields.Store(true)
	OptimizeVariablesExtraction.Store(true)
	BatchExtractedVariablesJSON.Store(true)
	DisableUploadFinding.Store(true)
	CacheRenderFieldPath.Store(true)
}
