package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/config"
	"github.com/udaykishore-resu/payments-platform/internal/domain/risk"
	"github.com/udaykishore-resu/payments-platform/internal/domain/routing"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/platform/tenantctx"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// SeedProfile names a synthetic dataset shape.
//
// The profiles differ in *scale and coverage*, not in kind: `minimal` is one tenant, one merchant
// and one gateway — enough to take a payment — and the larger ones add merchants in every
// lifecycle state so that a test can find a suspended one without creating it first.
type SeedProfile string

// The profiles `platformctl seed --profile` selects between; see config/seed/profiles.yaml.
const (
	SeedMinimal     SeedProfile = "minimal"
	SeedDev         SeedProfile = "dev"
	SeedIntegration SeedProfile = "integration"
	SeedE2E         SeedProfile = "e2e"
	SeedLoad        SeedProfile = "load"
)

// SeedOptions parameterises a seed run.
type SeedOptions struct {
	Profile SeedProfile
	// Scale multiplies the profile's base merchant count.
	Scale int
	// Seed is the PRNG seed. The same profile, scale and seed produce byte-identical data,
	// which is what lets a test assert on a specific merchant's configuration without querying
	// for it first, and what makes "reproduce it locally" a real instruction.
	Seed int64
	// Environment is the environment the seeded merchants and connections belong to.
	Environment shared.Environment
	// Gateways are the adapter slugs to register and connect every merchant to. The slugs must
	// match registered adapters, or the seeded merchants route to gateways this build cannot
	// dispatch through.
	Gateways []string
	// GatewayBaseURL is the endpoint the seeded catalogue rows point at, for this environment.
	// It is a required part of the fixture rather than an optional extra: an adapter with no
	// configured endpoint refuses at dispatch with GATEWAY_NOT_CONFIGURED, so a seeded database
	// without it produces a stack that comes up healthy and fails every payment.
	GatewayBaseURL string
	// Reset truncates the seeded tables first. Never permitted in production; the caller
	// enforces that, because the refusal belongs where an operator reads it.
	Reset bool
}

// SeedResult reports what was written.
type SeedResult struct {
	TenantID    string
	MerchantIDs []string
	Gateways    []string
	// SecretRefs are the credential references the seeded connections point at. They are
	// returned so the caller can write a matching local secrets document — the pair is what
	// makes `scripts/dev-up.sh` produce a stack that can actually take a payment.
	SecretRefs map[string]string
	Counts     map[string]int
}

// Seeder writes a deterministic synthetic dataset.
//
// # Why the data is generated and never copied
//
// deployment.md §6.1 states the rule; it is worth restating where the data is produced.
// Anonymising a relational payment dataset is not reliably achievable — merchant names,
// bank-account fragments, amounts, timestamps and gateway references re-identify in combination
// — so a "scrubbed" production dump is a breach that has not been noticed yet. There is no import
// path here, no `--from-dump`, and the caller refuses production outright.
//
// # Why it writes SQL rather than driving the aggregates
//
// A merchant reaches ACTIVE by walking an eleven-state lifecycle with a KYC decision, a bank
// validation and a certification run in the middle of it. Driving that from a seeder would mean
// either stubbing four external providers or asserting that the seeder's path through the FSM is
// the same one onboarding takes — and the second is a test of onboarding, not a fixture. Writing
// the end state directly keeps the fixture honest about being a fixture. The database's own CHECK
// constraints and foreign keys are still in force, so a seeder that wrote an impossible row fails
// here rather than producing data no code path could have created.
type Seeder struct {
	pool  *Pool
	clock shared.Clock
}

// NewSeeder builds the seeder.
func NewSeeder(pool *Pool, clock shared.Clock) *Seeder {
	if clock == nil {
		clock = shared.SystemClock{}
	}
	return &Seeder{pool: pool, clock: clock}
}

// seededTables is the truncation set for --reset, in dependency order.
//
// It is an explicit list rather than "every table in pp": a reset that truncated
// `schema_migrations` would make the database look unmigrated, and one that truncated
// `partition_registry` would detach every payment partition. Naming the tables makes the blast
// radius reviewable.
var seededTables = []string{
	"payment_attempts", "refunds", "payments",
	"gateway_connections", "gateway_health", "configuration_versions", "configurations",
	"merchants", "tenants", "outbox_events", "audit_records",
}

// Seed writes the dataset and returns what it produced.
func (s *Seeder) Seed(ctx context.Context, opts SeedOptions) (SeedResult, error) {
	if !opts.Environment.IsValid() {
		return SeedResult{}, apierror.Newf(apierror.CodeValidationFailed,
			"seed requires a valid environment, got %q", opts.Environment)
	}
	if len(opts.Gateways) == 0 {
		opts.Gateways = []string{"simulator"}
	}
	if opts.GatewayBaseURL == "" {
		opts.GatewayBaseURL = "http://127.0.0.1:9090"
	}
	count := merchantCount(opts.Profile, opts.Scale)
	now := s.clock.Now().UTC()

	if opts.Reset {
		for _, t := range seededTables {
			if _, err := s.pool.pool.Exec(ctx, "TRUNCATE pp."+t+" CASCADE"); err != nil {
				return SeedResult{}, mapError(err, "seed: truncate "+t)
			}
		}
	}

	out := SeedResult{
		Gateways:   opts.Gateways,
		SecretRefs: map[string]string{},
		Counts:     map[string]int{},
	}

	// Identifiers are derived from the seed rather than minted, which is what makes the dataset
	// byte-identical across runs. The timestamp component is the ULID's, so the IDs still sort
	// chronologically and still resolve to the partition their rows live in.
	out.TenantID = seedID("ten", opts.Seed, "tenant")

	if err := s.upsertTenant(ctx, out.TenantID, opts, now); err != nil {
		return SeedResult{}, err
	}
	for _, g := range opts.Gateways {
		if err := s.upsertGateway(ctx, g, opts, now); err != nil {
			return SeedResult{}, err
		}
		if err := s.upsertHealth(ctx, g, now); err != nil {
			return SeedResult{}, err
		}
	}

	for i := 0; i < count; i++ {
		merchantID := seedID("mrc", opts.Seed, fmt.Sprintf("merchant-%d", i))
		status := merchantStatusFor(opts.Profile, i)
		if err := s.upsertMerchant(ctx, out.TenantID, merchantID, i, status, opts, now); err != nil {
			return SeedResult{}, err
		}
		out.MerchantIDs = append(out.MerchantIDs, merchantID)

		for _, g := range opts.Gateways {
			connID := seedID("gwc", opts.Seed, fmt.Sprintf("conn-%d-%s", i, g))
			ref := fmt.Sprintf("secret://%s/%s/%s/%s", opts.Environment, out.TenantID, merchantID, g)
			if err := s.upsertConnection(ctx, connID, out.TenantID, merchantID, g, ref, opts, now); err != nil {
				return SeedResult{}, err
			}
			out.SecretRefs[merchantID+"/"+g] = ref
		}
		if err := s.publishConfig(ctx, out.TenantID, merchantID, opts, now); err != nil {
			return SeedResult{}, err
		}
	}

	reports := NewOperatorReports(s.pool)
	counts, err := reports.TableCounts(ctx, []string{"tenants", "merchants", "gateways",
		"gateway_connections", "configurations", "configuration_versions", "payments"})
	if err != nil {
		return SeedResult{}, err
	}
	out.Counts = counts
	return out, nil
}

// merchantCount is the profile's merchant count, multiplied by the scale.
func merchantCount(p SeedProfile, scale int) int {
	if scale <= 0 {
		scale = 1
	}
	switch p {
	case SeedMinimal:
		return 1
	case SeedE2E:
		return 2 * scale
	case SeedIntegration:
		return 2 * scale
	case SeedLoad:
		return 10 * scale
	case SeedDev:
		return scale
	default:
		// An unknown profile seeds the dev shape rather than nothing: a fixture command that
		// silently produced an empty database because of a typo in --profile is a fifteen-minute
		// debugging session that ends in embarrassment.
		return scale
	}
}

// merchantStatusFor spreads merchants across lifecycle states.
//
// The first merchant is always ACTIVE, and that is deliberate rather than incidental: a seeded
// database whose first merchant happened to be SUSPENDED would make every getting-started
// instruction fail, and the person following it would conclude the platform is broken. Beyond the
// first, the larger profiles include a suspended and a terminated merchant so that a test needing
// one does not have to create it.
func merchantStatusFor(p SeedProfile, i int) string {
	if i == 0 || p == SeedMinimal {
		return "ACTIVE"
	}
	switch i % 8 {
	case 5:
		return "SUSPENDED"
	case 6:
		return "KYC_PENDING"
	case 7:
		return "TERMINATED"
	default:
		return "ACTIVE"
	}
}

func (s *Seeder) upsertTenant(ctx context.Context, id string, opts SeedOptions, now time.Time) error {
	const q = `
INSERT INTO pp.tenants (
    tenant_id, name, tier, status, residency_region, environments,
    enabled_gateways, enabled_currencies, enabled_methods,
    max_merchants, requests_per_second, concurrent_payments, cache_memory_mb,
    created_at, updated_at)
VALUES ($1,$2,'POOLED','ACTIVE','GLOBAL',ARRAY[$3]::TEXT[],$4,$5,$6,10000,5000,512,256,$7,$7)
ON CONFLICT (tenant_id) DO UPDATE SET
    enabled_gateways   = EXCLUDED.enabled_gateways,
    enabled_currencies = EXCLUDED.enabled_currencies,
    enabled_methods    = EXCLUDED.enabled_methods,
    updated_at         = EXCLUDED.updated_at`
	_, err := s.pool.pool.Exec(ctx, q, id, "Seeded Tenant", string(opts.Environment),
		opts.Gateways, seedCurrencies, seedMethods, now)
	return mapError(err, "seed: tenant")
}

// seedCurrencies and seedMethods are what the seeded tenant and merchants are entitled to. They
// are the platform's most common corridors rather than everything, because a fixture that enables
// every currency makes an "is this currency enabled?" test vacuous.
var (
	seedCurrencies = []string{"EUR", "USD", "GBP"}
	seedMethods    = []string{"CARD"}
)

func (s *Seeder) upsertGateway(ctx context.Context, id string, opts SeedOptions, now time.Time) error {
	const q = `
INSERT INTO pp.gateways (
    gateway_id, display_name, vendor, api_version, base_urls, capabilities, cost_model,
    signature_scheme, status, regions, created_at, updated_at)
VALUES ($1,$2,$1,'v1',$4::jsonb,$3::jsonb,$5::jsonb,'HMAC_SHA256','ACTIVE',
        ARRAY['EU','US']::TEXT[],$6,$6)
ON CONFLICT (gateway_id) DO UPDATE SET
    base_urls    = EXCLUDED.base_urls,
    capabilities = EXCLUDED.capabilities,
    cost_model   = EXCLUDED.cost_model,
    status       = 'ACTIVE',
    updated_at   = EXCLUDED.updated_at`
	// The capability document is what the routing engine's hard filters read. Declaring exactly
	// what the built-in adapters implement matters: a descriptor that claims a capability its
	// adapter does not implement routes traffic to a gateway that will refuse it, which the
	// registry refuses to construct — so an over-generous fixture turns into a startup failure
	// rather than a silent mis-route.
	//
	// The field names are capabilitiesDTO's, because that is what reads this column back. A
	// hand-written document with plausible-but-different names decodes to a descriptor with every
	// capability false, which the routing engine then filters out entirely — a fixture bug that
	// presents as NO_ELIGIBLE_GATEWAY and takes an afternoon.
	caps, err := json.Marshal(capabilitiesDTO{
		Currencies: []string{"EUR", "USD", "GBP"},
		Methods:    []string{"CARD"},
		// The licensed corridors are listed explicitly rather than left empty. An empty country
		// list is *not* "everywhere": the routing engine reads it as "this gateway is licensed
		// nowhere" and excludes the candidate, because an acquiring licence that nobody recorded
		// is not a licence. The seeded set is the corridors the rest of the fixture uses.
		Countries:                []string{"DE", "FR", "NL", "GB", "US", "IE", "ES", "IT"},
		Operations:               []string{"authorize", "capture", "refund", "void", "lookup"},
		SupportsPartialCapture:   true,
		SupportsMultipleCaptures: true,
		SupportsPartialRefund:    true,
		SupportsVoid:             true,
		Supports3DS2:             true,
		SupportsIdempotencyKeys:  true,
		MaxRefundWindowDays:      180,
		// Below the platform's own 168-hour authorization validity, so the platform expires a
		// hold before the gateway silently does — which is the ordering that keeps a capture from
		// being attempted against an authorization the issuer has already released.
		AuthorizationValidityHours: 168,
	})
	if err != nil {
		return apierror.Wrap(err, apierror.CodeInternalError, "seed: encode gateway capabilities")
	}
	urls, err := json.Marshal(map[string]string{string(opts.Environment): opts.GatewayBaseURL})
	if err != nil {
		return apierror.Wrap(err, apierror.CodeInternalError, "seed: encode gateway base URLs")
	}
	// A price list, not an empty one. Cost is an input to the routing score, and the assembler
	// treats a gateway it cannot price as unroutable — "an unpriced gateway is not free", because
	// leaving the cost at zero would make the gateway nobody has priced the cheapest candidate in
	// every decision. An empty `{"rates":[]}` therefore excludes the gateway from every candidate
	// set, which presents as NO_ELIGIBLE_GATEWAY and is one of the least obvious ways a seeded
	// platform can look broken.
	//
	// The numbers are published card-present-style pricing, per currency: 2.90% plus a fixed fee.
	rates := make([]costRateDTO, 0, len(seedCurrencies))
	for _, cur := range seedCurrencies {
		rates = append(rates, costRateDTO{
			Currency: cur, Method: "CARD", BasisPoints: 290, FixedFee: 30,
		})
	}
	cost, err := json.Marshal(costModelDTO{Rates: rates})
	if err != nil {
		return apierror.Wrap(err, apierror.CodeInternalError, "seed: encode gateway cost model")
	}
	_, err = s.pool.pool.Exec(ctx, q, id, strings.ToUpper(id[:1])+id[1:], caps, urls, cost, now)
	return mapError(err, "seed: gateway")
}

// healthSeededOperations are the operations the seeded gateways are recorded HEALTHY for.
//
// The routing engine treats an *absent* health row as "unmeasured", and unmeasured excludes the
// gateway from every candidate set — correctly, because a gateway nobody has observed is not
// known to work, and dispatching to it on a hope is how a fleet-wide restart during an outage
// sends a thundering herd at a dead vendor. A seeded dataset with no health rows therefore
// produces NO_ELIGIBLE_GATEWAY on every payment, which reads as a routing bug rather than as a
// missing fixture. Writing the rows is what makes the seeded platform usable.
var healthSeededOperations = []string{"authorize", "capture", "refund", "void", "lookup"}

func (s *Seeder) upsertHealth(ctx context.Context, gatewayID string, now time.Time) error {
	const q = `
INSERT INTO pp.gateway_health (
    gateway_id, operation, state, error_rate, p99_latency_ms, sample_count,
    window_started_at, last_observed_at, state_changed_at)
VALUES ($1,$2,'HEALTHY',0,120,100,$3,$3,$3)
ON CONFLICT (gateway_id, operation) DO UPDATE SET
    state            = 'HEALTHY',
    error_rate       = 0,
    last_observed_at = EXCLUDED.last_observed_at`
	for _, op := range healthSeededOperations {
		if _, err := s.pool.pool.Exec(ctx, q, gatewayID, op, now); err != nil {
			return mapError(err, "seed: gateway health")
		}
	}
	return nil
}

func (s *Seeder) upsertMerchant(ctx context.Context, tenantID, merchantID string, index int,
	status string, opts SeedOptions, now time.Time) error {
	const q = `
INSERT INTO pp.merchants (
    merchant_id, tenant_id, external_reference, legal_name, display_name, environment,
    status, kyc_status, risk_rating, active_config_version, created_at, updated_at, activated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'STANDARD',1,$9,$9,$10)
ON CONFLICT (merchant_id) DO UPDATE SET
    status                = EXCLUDED.status,
    active_config_version = EXCLUDED.active_config_version,
    updated_at            = EXCLUDED.updated_at,
    activated_at          = EXCLUDED.activated_at`
	var activatedAt *time.Time
	kyc := "NOT_STARTED"
	if status == "ACTIVE" || status == "SUSPENDED" || status == "TERMINATED" {
		activatedAt = &now
		kyc = "APPROVED"
	}
	name := fmt.Sprintf("Seeded Merchant %02d", index+1)
	_, err := s.pool.pool.Exec(ctx, q, merchantID, tenantID,
		fmt.Sprintf("seed-%03d", index+1), name+" Ltd", name,
		string(opts.Environment), status, kyc, now, activatedAt)
	if err != nil {
		return mapError(err, "seed: merchant")
	}
	if status == "TERMINATED" {
		_, err = s.pool.pool.Exec(ctx,
			`UPDATE pp.merchants SET terminated_at = $2 WHERE merchant_id = $1 AND terminated_at IS NULL`,
			merchantID, now)
	}
	return mapError(err, "seed: merchant lifecycle timestamps")
}

func (s *Seeder) upsertConnection(ctx context.Context, connID, tenantID, merchantID, gatewayID,
	secretRef string, opts SeedOptions, now time.Time) error {
	const q = `
INSERT INTO pp.gateway_connections (
    connection_id, tenant_id, merchant_id, gateway_id, environment, status,
    certification_status, certification_report_id, external_account_ref, credential_ref,
    webhook_registration_id, webhook_endpoint, provisioned_at, certified_at,
    created_at, updated_at)
VALUES ($1,$2,$3,$4,$5,'CERTIFIED','PASSED',$6,$7,$8,$9,$10,$11,$11,$11,$11)
ON CONFLICT (connection_id) DO UPDATE SET
    status                  = EXCLUDED.status,
    certification_status    = EXCLUDED.certification_status,
    certification_report_id = EXCLUDED.certification_report_id,
    credential_ref          = EXCLUDED.credential_ref,
    webhook_registration_id = EXCLUDED.webhook_registration_id,
    updated_at              = EXCLUDED.updated_at`
	// CERTIFIED is the status that lets money flow, and the database's
	// connection_certified_is_complete constraint requires every precondition for it to be a
	// present column: a certification report, a credential reference and a webhook registration.
	// The seeder supplies all three rather than asserting CERTIFIED with the rest blank, which
	// the constraint exists to make impossible — a fixture that could sidestep it would be a
	// fixture that produced state no real onboarding could reach.
	_, err := s.pool.pool.Exec(ctx, q, connID, tenantID, merchantID, gatewayID,
		string(opts.Environment),
		seedID("crt", opts.Seed, "cert-"+merchantID+"-"+gatewayID),
		"acct_"+gatewayID+"_"+merchantID[len(merchantID)-6:],
		secretRef,
		"whreg_"+gatewayID+"_"+merchantID[len(merchantID)-6:],
		"https://webhooks.example.test/v1/webhooks/"+gatewayID,
		now)
	return mapError(err, "seed: gateway connection")
}

// publishConfig writes an ACTIVE configuration version through the repository.
//
// The repository rather than raw SQL, because the document column is `json.Marshal` of the domain
// aggregate and its checksum is verified on every read — a hand-written JSON document that drifted
// from the struct would load as "configuration corrupt", which is a confusing way to discover a
// fixture bug.
func (s *Seeder) publishConfig(ctx context.Context, tenantID, merchantID string,
	opts SeedOptions, now time.Time) error {
	published := now
	cfg := &config.MerchantConfig{
		MerchantID:  shared.MerchantID(merchantID),
		TenantID:    shared.TenantID(tenantID),
		Version:     1,
		Status:      config.StatusActive,
		Environment: opts.Environment,
		SupportedCurrencies: []money.Currency{
			money.Currency("EUR"), money.Currency("USD"), money.Currency("GBP"),
		},
		PaymentMethods: []shared.PaymentMethod{shared.MethodCard},
		Routing: routing.Policy{
			Strategy: routing.StrategyPriorityWithFallback,
			Rules: []routing.Rule{{
				ID:     "rule-1",
				Action: routing.Action{Primary: shared.GatewayID(opts.Gateways[0])},
			}},
		},
		Risk: risk.Policy{
			MaxTransactionAmount: money.MustNew(500000, "EUR"),
			DailyVolumeLimit:     money.MustNew(50000000, "EUR"),
			// Every money field is given a currency, including the ones a fixture is tempted to
			// leave zero. money.Money's zero value is a valid amount in an *invalid* currency, and
			// it round-trips through JSON as `""` — which the decoder refuses, so the whole
			// configuration document becomes unreadable and the pod fails readiness with an error
			// that names the currency rather than the field.
			Require3DSAbove: money.MustNew(30000, "EUR"),
			Velocity:        risk.Velocity{MaxPaymentsPerMinute: 600, MaxPerCardPerHour: 100},
		},
		Limits: config.Limits{
			MaxRefundWindowDays:        180,
			MaxPartialCaptures:         4,
			MaxRefundsPerPayment:       8,
			AuthorizationValidityHours: 168,
		},
		FeatureFlags: map[string]bool{},
		CreatedAt:    now,
		CreatedBy:    "platformctl seed",
		PublishedAt:  &published,
		Comment:      "seeded configuration",
	}
	if len(opts.Gateways) > 1 {
		for _, g := range opts.Gateways[1:] {
			cfg.Routing.Rules[0].Action.Fallbacks = append(
				cfg.Routing.Rules[0].Action.Fallbacks, shared.GatewayID(g))
		}
	}

	repo := &ConfigRepository{q: s.pool.pool, tenant: shared.TenantID(tenantID)}
	// The repository refuses to query without a tenant on the context — that guard is what makes
	// a cross-tenant write impossible on the request path, and the seeder is not exempt from it.
	ctx, err := tenantctx.WithTenant(ctx, tenantctx.TenantContext{
		TenantID:    shared.TenantID(tenantID),
		Tier:        shared.TierPooled,
		Environment: opts.Environment,
		Source:      tenantctx.SourceToken,
		Principal:   tenantctx.Principal{Type: tenantctx.PrincipalHuman, ID: "platformctl", Name: "platformctl seed"},
	})
	if err != nil {
		return err
	}
	// expectedVersion 0 is "there is no head row yet". A re-seed of the same dataset therefore
	// conflicts rather than silently publishing a second version, which is the correct behaviour:
	// re-running seed without --reset must not quietly double a merchant's configuration history.
	err = repo.Publish(ctx, cfg, 0)
	if apierror.CodeOf(err) == apierror.CodeConfigurationVersionConflict {
		return nil
	}
	return err
}

// seedID derives a deterministic, well-formed platform identifier.
//
// Deterministic because the whole value of a seeded dataset is that the same profile, scale and
// seed produce the same identifiers: a test can then assert on a specific merchant without first
// querying for one, and a runbook can name the merchant it wants you to look at.
//
// The encoding is Crockford Base32 with the platform's alphabet, so the result passes pkg/ids'
// parser and the database's own CHECK constraints — a fixture that produced an identifier the
// platform refuses is a fixture that fails at the insert with a message about a regular
// expression.
func seedID(prefix string, seed int64, label string) string {
	var buf [8]byte
	// A two's-complement reinterpretation, not a numeric conversion: these eight bytes are hash
	// input and never a quantity, so a negative --seed is as legitimate as a positive one.
	binary.BigEndian.PutUint64(buf[:], uint64(seed))
	sum := sha256.Sum256(append(buf[:], label...))

	const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	var b strings.Builder
	b.WriteString(prefix)
	b.WriteByte('_')
	// The first symbol carries only the top two bits of the 48-bit timestamp — 26 symbols is 130
	// bits for a 128-bit value — so a ULID whose first symbol exceeds '7' has a timestamp that
	// overflowed and is not legal. pkg/ids rejects it, and so does the database's CHECK. Masking
	// the first symbol into 0..7 is what keeps a *derived* identifier a valid one.
	b.WriteByte(crockford[int(sum[0])%8])
	for i := 1; i < 26; i++ {
		b.WriteByte(crockford[int(sum[i%len(sum)])%len(crockford)])
	}
	return b.String()
}

// SeededTables is the list --reset truncates, exported so the CLI can print it before doing it.
//
// A destructive flag whose blast radius is only visible in the source is a flag an operator
// cannot evaluate under pressure. Printing the list is what turns "--reset" into a decision.
func SeededTables() []string {
	out := make([]string, len(seededTables))
	copy(out, seededTables)
	return out
}
