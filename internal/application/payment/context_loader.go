package payment

import (
	"context"
	"sync"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/config"
	"github.com/udaykishore-resu/payments-platform/internal/domain/gateway"
	"github.com/udaykishore-resu/payments-platform/internal/domain/merchant"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// DefaultMaxConfigStaleness is the fail-static cliff from baseline §15.
//
// Fifteen minutes is not a round number chosen for tidiness: it is longer than any plausible
// control-plane deployment, longer than a Kafka partition rebalance, and shorter than the window
// in which a merchant suspension or a sanctions-list update must take effect. Serving beyond it
// is not "slightly stale", it is processing under rules nobody can vouch for.
const DefaultMaxConfigStaleness = 15 * time.Minute

// ContextLoaderDeps is what assembling a merchant snapshot requires.
type ContextLoaderDeps struct {
	// UoW is used for the merchant and connection reads. They come from the database rather than
	// from the configuration snapshot because they are *registry* facts, not policy: a
	// suspension must stop payments within seconds, and a merchant row is small, indexed and
	// cached at the connection-pool layer.
	UoW ports.UnitOfWork
	// Config is the fail-static snapshot, deliberately not ConfigurationRepository. See the
	// doc comment on ports.ConfigProvider: giving the payment path the repository is how a
	// synchronous control-plane read ends up in the money path without anyone noticing.
	Config ports.ConfigProvider
	Clock  shared.Clock
	// MaxStaleness is the cliff. Zero means DefaultMaxConfigStaleness.
	MaxStaleness time.Duration
}

// ContextLoader is the production MerchantContextLoader.
//
// It owns one decision that the rest of the platform depends on and that is easy to get subtly
// wrong: what to do when the control plane has been unreachable for a while. The answer,
// baseline §15, is neither "stop" nor "carry on regardless" but a cliff with an asymmetry:
//
//   - Inside the tolerance, serve from the snapshot. A stale-by-seconds configuration is what
//     eventual consistency looks like and is not an incident.
//   - Past the tolerance, keep serving **merchants already in the snapshot** and refuse
//     merchants that are not. A merchant we have a configuration for is a merchant whose limits,
//     blocked countries and enabled currencies we can still enforce, even if the document is an
//     hour old; a merchant we have never seen has no limits at all, and processing for them
//     would mean processing with no policy whatsoever.
//
// The asymmetry is the whole design. Refusing everyone converts a control-plane outage into a
// total payment outage — the data plane's independence (ADR-007) exists precisely to prevent
// that. Serving everyone means the first payment for an unknown merchant is processed with an
// empty policy, which is worse than a retryable 503 by every measure.
type ContextLoader struct {
	deps ContextLoaderDeps

	// mu guards known. The map is the loader's own record of which merchants it has successfully
	// resolved a configuration for; it is what "already in the snapshot" means operationally,
	// and it is deliberately process-local — a replica that has never served a merchant has no
	// business claiming it can serve them from a snapshot it does not have.
	mu    sync.RWMutex
	known map[shared.MerchantID]MerchantSnapshot
}

// NewContextLoader constructs the loader.
func NewContextLoader(d ContextLoaderDeps) *ContextLoader {
	if d.Clock == nil {
		d.Clock = shared.SystemClock{}
	}
	if d.MaxStaleness <= 0 {
		d.MaxStaleness = DefaultMaxConfigStaleness
	}
	return &ContextLoader{deps: d, known: make(map[shared.MerchantID]MerchantSnapshot)}
}

// requestCacheKey scopes the per-request memo. A struct{} key type rather than a string is the
// standard defence against another package colliding with ours in the same context.
type requestCacheKey struct{}

type requestCache struct {
	mu   sync.Mutex
	seen map[shared.MerchantID]MerchantSnapshot
}

// WithRequestCache returns a context carrying a per-request snapshot memo.
//
// A payment touches the merchant context three or four times — the create path, the validator,
// the risk evaluator, the candidate builder — and every one of them must see the *same* answer.
// Without the memo, a configuration publish landing mid-request would produce a payment
// validated against one version and routed under another, which is a decision nobody took. The
// memo lives on the context rather than on the loader because its lifetime is the request's, and
// a cache on the loader would leak across requests and across tenants.
func WithRequestCache(ctx context.Context) context.Context {
	if _, ok := ctx.Value(requestCacheKey{}).(*requestCache); ok {
		return ctx
	}
	return context.WithValue(ctx, requestCacheKey{}, &requestCache{seen: map[shared.MerchantID]MerchantSnapshot{}})
}

func cacheFrom(ctx context.Context) *requestCache {
	c, _ := ctx.Value(requestCacheKey{}).(*requestCache)
	return c
}

// Load assembles the merchant snapshot.
func (l *ContextLoader) Load(ctx context.Context, id shared.MerchantID) (MerchantSnapshot, error) {
	if id.IsZero() {
		return MerchantSnapshot{}, apierror.New(apierror.CodeMerchantNotFound,
			"a merchant identifier is required")
	}
	if c := cacheFrom(ctx); c != nil {
		c.mu.Lock()
		snap, ok := c.seen[id]
		c.mu.Unlock()
		if ok {
			return snap, nil
		}
	}

	age := l.deps.Config.SnapshotAge()
	if age > l.deps.MaxStaleness {
		// Past the cliff. A merchant we have served before is served again from the last known
		// good snapshot; everyone else is refused with a retryable error that names the age, so
		// that the on-call engineer reading it knows to look at the control plane rather than at
		// the merchant.
		l.mu.RLock()
		snap, ok := l.known[id]
		l.mu.RUnlock()
		if !ok {
			return MerchantSnapshot{}, staleConfigError(age, l.deps.MaxStaleness)
		}
		snap.SnapshotAge = age
		l.memo(ctx, id, snap)
		return snap, nil
	}

	snap, err := l.assemble(ctx, id, age)
	if err != nil {
		return MerchantSnapshot{}, err
	}
	l.mu.Lock()
	l.known[id] = snap
	l.mu.Unlock()
	l.memo(ctx, id, snap)
	return snap, nil
}

func (l *ContextLoader) memo(ctx context.Context, id shared.MerchantID, snap MerchantSnapshot) {
	if c := cacheFrom(ctx); c != nil {
		c.mu.Lock()
		c.seen[id] = snap
		c.mu.Unlock()
	}
}

// assemble performs the three reads and flattens them.
//
// The merchant and the connections are read inside one transaction so that they describe one
// instant: a connection revoked between the two reads would otherwise produce a snapshot in
// which a merchant is active and holds a credential that no longer exists.
func (l *ContextLoader) assemble(ctx context.Context, id shared.MerchantID, age time.Duration) (MerchantSnapshot, error) {
	var m *merchant.Merchant
	var conns []*gateway.Connection
	if err := l.deps.UoW.Within(ctx, func(ctx context.Context, r ports.Repositories) error {
		var err error
		if m, err = r.Merchants.Get(ctx, id); err != nil {
			return err
		}
		conns, err = r.Connections.ListForMerchant(ctx, id)
		return err
	}); err != nil {
		return MerchantSnapshot{}, err
	}
	if m == nil {
		return MerchantSnapshot{}, apierror.Newf(apierror.CodeMerchantNotFound,
			"merchant %s does not exist under your tenant", id)
	}

	snap := MerchantSnapshot{
		MerchantID:  m.ID(),
		TenantID:    m.TenantID(),
		Environment: m.Environment(),
		Country:     m.Profile().Country,
		RiskRating:  string(m.RiskRating()),
		Status:      m.Status(),
		SnapshotAge: age,
		Connections: map[shared.GatewayID]ConnectionSnapshot{},
	}
	for _, c := range conns {
		if c == nil {
			continue
		}
		snap.Connections[c.GatewayID()] = ConnectionSnapshot{
			ConnectionID:      c.ID(),
			GatewayID:         c.GatewayID(),
			ExternalAccountID: c.ExternalAccountRef(),
			Status:            c.Status(),
			Certified:         c.CertificationStatus() == gateway.CertificationPassed,
			SecretRef:         c.CredentialRef(),
		}
	}

	cfg, err := l.deps.Config.Get(ctx, id)
	if err != nil {
		// A configuration that cannot be resolved at all is not a degraded read; it is the
		// absence of a policy. L5's CONFIG_SNAPSHOT_FRESH_ENOUGH fails closed on it, and leaving
		// ConfigPresent false is how that rule is told.
		return snap, nil //nolint:nilerr // an unresolvable configuration is the absence of a policy, not a read failure; leaving ConfigPresent false is how L5's fail-closed rule is told
	}
	if cfg != nil {
		applyConfig(&snap, cfg)
	}
	return snap, nil
}

// applyConfig flattens the configuration document onto the snapshot.
func applyConfig(snap *MerchantSnapshot, cfg *config.MerchantConfig) {
	snap.ConfigPresent = true
	snap.ConfigVersion = cfg.Version
	snap.SupportedCurrencies = append([]money.Currency(nil), cfg.SupportedCurrencies...)
	snap.PaymentMethods = append([]shared.PaymentMethod(nil), cfg.PaymentMethods...)
	snap.SupportedCountries = append([]shared.Country(nil), cfg.Countries...)
	snap.Routing = cfg.Routing
	snap.Risk = cfg.Risk
	snap.MaxRefundWindow = time.Duration(cfg.Limits.MaxRefundWindowDays) * 24 * time.Hour
	snap.MaxPartialCaptures = cfg.Limits.MaxPartialCaptures
	snap.FeatureFlags = cfg.FeatureFlags
	// Manual capture is permitted when the merchant's limits allow more than a single capture,
	// or when the document declares no partial-capture limit at all. A zero limit is "not
	// configured" everywhere else in this platform, and treating it as "zero captures permitted"
	// here would silently disable manual capture for every merchant who omitted the field.
	snap.ManualCaptureAllowed = cfg.Limits.MaxPartialCaptures >= 0
	snap.RoutableCombinations = routableCombinations(cfg)
}

// routableCombinations compiles the (method, currency) coverage of the configuration.
//
// The country is left as the wildcard. L4 already proved at publish time that every enabled
// (currency, method) pair is servable by some gateway in the routing policy
// (L4.EVERY_COMBINATION_ROUTABLE), so re-deriving per-country coverage here would be re-running
// a control-plane check on the hot path — and doing it against today's descriptors rather than
// the ones that were checked, which is a different computation wearing the same name.
func routableCombinations(cfg *config.MerchantConfig) []RouteCombination {
	if len(cfg.Routing.ReferencedGateways()) == 0 {
		return nil
	}
	out := make([]RouteCombination, 0, len(cfg.SupportedCurrencies)*len(cfg.PaymentMethods))
	for _, cur := range cfg.SupportedCurrencies {
		for _, m := range cfg.PaymentMethods {
			out = append(out, RouteCombination{Method: m, Currency: cur})
		}
	}
	return out
}

// Invalidate drops a merchant from the last-known-good set.
//
// It exists for the priority path that handles merchant.suspended.v1: a suspension must not be
// survivable by the fail-static behaviour, or a merchant suspended during a control-plane outage
// would keep processing on the strength of a snapshot taken before they were stopped.
func (l *ContextLoader) Invalidate(id shared.MerchantID) {
	l.mu.Lock()
	delete(l.known, id)
	l.mu.Unlock()
	l.deps.Config.Invalidate(id)
}
