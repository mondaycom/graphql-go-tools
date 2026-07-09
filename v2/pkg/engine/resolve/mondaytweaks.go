// This file defines compile-time feature flags for monday.com-specific
// behavioural overrides in graphql-go-tools, mirroring the pattern of
// cosmo/router/pkg/mondaytweaks. Keep only performance/memory toggles here.
package resolve

const (
	// EnableSizedArenaPool switches request arenas to the size-classed,
	// retention-bounded pool (see sizedArenaPool). Flip to false to fall back
	// to the upstream arena.Pool in case a workload regresses.
	EnableSizedArenaPool = true
)
