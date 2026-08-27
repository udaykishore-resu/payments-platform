package registry

import (
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/adyen"
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/paypal"
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/simulator"
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/stripe"
)

// BuiltIn returns the factories this binary ships with.
//
// It is a function returning a slice rather than a package-level `init()` that registers into a
// global. Two reasons, both about testability:
//
//   - An init-time global means every test binary that links this package gets every adapter,
//     whether it wants them or not, and a test cannot construct a registry with only the simulator.
//   - Registration order and failure become invisible. A slice can be inspected, filtered and
//     asserted on; an init cannot.
//
// This is also the *only* place in the repository where the four gateways are named together.
// Adding a fifth is one line here, not an edit to a switch statement in the orchestrator — and
// registry_test.go fails the build if such a switch reappears.
func BuiltIn(sim *simulator.Engine) []spi.Factory {
	return []spi.Factory{
		stripe.NewFactory(),
		adyen.NewFactory(),
		paypal.NewFactory(),
		simulator.NewFactory(sim),
	}
}

// NewWithBuiltIn returns a registry with every shipped adapter registered and nothing configured.
//
// Configuration is separate on purpose: the factories are compiled in and never change, while the
// records come from the gateway registry table and are reloaded while the process runs. A
// constructor that did both would make a configuration reload require re-registration, and
// re-registration is an error by design.
func NewWithBuiltIn(sim *simulator.Engine) (*Registry, error) {
	r := New()
	for _, f := range BuiltIn(sim) {
		if err := r.Register(f); err != nil {
			return nil, err
		}
	}
	return r, nil
}
