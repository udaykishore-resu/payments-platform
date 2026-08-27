// Package rules is the aggregation point for the seven rule levels.
//
// It exists for one reason: something has to import every level so that every level's init has
// run before the registry is read. Without it, `registry.All()` returns whatever subset of the
// catalog the calling binary happened to link, and the documentation-consistency check becomes
// a check that the rules you remembered to import are documented — which is the property it is
// least useful to guarantee.
//
// Import this package (for its side effect, or for Catalog) anywhere the whole catalog must be
// visible: the CI checks, the documentation generator, and `platformctl validation list`.
package rules

import (
	"github.com/udaykishore-resu/payments-platform/internal/validation/engine"

	_ "github.com/udaykishore-resu/payments-platform/internal/validation/rules/l1api"
	_ "github.com/udaykishore-resu/payments-platform/internal/validation/rules/l2merchant"
	_ "github.com/udaykishore-resu/payments-platform/internal/validation/rules/l3gateway"
	_ "github.com/udaykishore-resu/payments-platform/internal/validation/rules/l4config"
	_ "github.com/udaykishore-resu/payments-platform/internal/validation/rules/l5payment"
	_ "github.com/udaykishore-resu/payments-platform/internal/validation/rules/l6response"
	_ "github.com/udaykishore-resu/payments-platform/internal/validation/rules/l7domain"
)

// Catalog returns every registered rule across all seven levels, sorted by ID.
func Catalog() []engine.Registration { return engine.Registry.All() }

// CatalogForLevel returns the registered rules for one level, sorted by ID.
func CatalogForLevel(level int) []engine.Registration { return engine.Registry.ForLevel(level) }

// Count returns the total number of registered rules. The catalog test asserts this against
// the total documented in docs/validation-plane.md §3.8.
func Count() int { return engine.Registry.Count() }
