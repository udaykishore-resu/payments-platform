//go:build temporal

package temporal

import "github.com/udaykishore-resu/payments-platform/internal/workflows/engine"

// This file exists to fail the build, not to run.
//
// The adapter is excluded from the default build because the Temporal SDK is not a dependency of
// this module, which means a change to the engine.Engine port would otherwise be discovered on
// the day somebody first tries `-tags temporal` — most likely the day they are trying to migrate
// off Postgres under time pressure. The assertion below turns that into a compile error in the
// tagged build instead.
var _ engine.Engine = (*Engine)(nil)
