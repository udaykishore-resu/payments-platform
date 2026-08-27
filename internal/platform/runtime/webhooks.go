package runtime

import (
	"context"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/registry"
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	appwebhook "github.com/udaykishore-resu/payments-platform/internal/application/webhook"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/secrets"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// WebhookSigningRef builds the reference to a gateway's inbound webhook signing secret.
//
// # Why this is platform-scoped rather than merchant-scoped
//
// The other secret references in this platform name a merchant, because the credential belongs to
// one merchant's connection. An *inbound* webhook signing secret does not: the gateway signs the
// delivery with the secret it issued for our endpoint, and the endpoint is one per gateway per
// environment. More to the point, the ingress cannot scope by merchant even if it wanted to —
// signature verification happens before the body is parsed (that ordering is the security
// property), so at the moment the secret is needed the merchant is not yet known.
//
// The `platform` segment occupies the merchant position deliberately rather than being omitted.
// Keeping the path shape identical means one IAM path-prefix scheme covers both kinds of secret,
// and an operator reading `/{env}/platform/stripe/webhook_signing` in a CloudTrail event can see
// at a glance that it is a platform secret and not a merchant's.
func WebhookSigningRef(env shared.Environment, gateway shared.GatewayID) string {
	return secrets.Scheme + string(env) + "/platform/" + gateway.String() + "/webhook_signing"
}

// WebhookVerifiers adapts the gateway adapter registry to the ingester's VerifierSource.
//
// The ingester declares its own one-method interface rather than taking the registry, and that
// separation has a security consequence rather than an aesthetic one: the webhook ingress is the
// most exposed surface in the platform, and the blast radius of compromising it must not include
// the ability to initiate payments. This adapter hands it verification and nothing else.
type WebhookVerifiers struct{ reg *registry.Registry }

// NewWebhookVerifiers wraps the registry.
func NewWebhookVerifiers(reg *registry.Registry) *WebhookVerifiers {
	return &WebhookVerifiers{reg: reg}
}

// Verifier returns the adapter that can authenticate this gateway's webhooks.
func (v *WebhookVerifiers) Verifier(ctx context.Context, g shared.GatewayID) (spi.WebhookVerifier, error) {
	return v.reg.ResolveVerifier(ctx, g)
}

// WebhookSecrets resolves a gateway's inbound signing secrets through the secrets provider.
//
// # Why it returns more than one
//
// Gateways rotate their own signing secrets on their own schedule, and during the window both the
// old and the new one produce valid signatures on deliveries already in flight. A verifier given
// only the current secret rejects every webhook signed with the previous one, the gateway retries
// them, they fail again, and the gateway eventually disables the endpoint — an outage caused
// entirely by our own rotation handling. Returning current-then-previous is what makes the
// overlap survivable, and the order matters: the common case must be the first comparison so that
// the constant-time check runs once rather than twice.
type WebhookSecrets struct {
	secrets     ports.SecretsProvider
	environment shared.Environment
}

// NewWebhookSecrets builds the source.
func NewWebhookSecrets(p ports.SecretsProvider, env shared.Environment) *WebhookSecrets {
	return &WebhookSecrets{secrets: p, environment: env}
}

// SigningSecrets returns the gateway's current signing secret followed by the previous one, where
// a previous one exists.
//
// A missing previous version is not an error: the overwhelmingly common state is a gateway whose
// secret has never been rotated, and treating that as a failure would make every fresh
// installation reject every webhook.
func (w *WebhookSecrets) SigningSecrets(ctx context.Context, g shared.GatewayID) ([]string, error) {
	ref := WebhookSigningRef(w.environment, g)
	current, err := w.secrets.Get(ctx, ref)
	if err != nil {
		return nil, err
	}
	out := valuesOf(current)
	if len(out) == 0 {
		return nil, apierror.Newf(apierror.CodeGatewayNotConfigured,
			"the webhook signing secret for gateway %s resolved to no fields", g)
	}
	// The previous version is read by staging label rather than by version number, because the
	// ingress does not track version numbers and must not have to: AWSPREVIOUS is exactly "the one
	// that was current before the last rotation", which is the set the gateway may still be
	// signing with.
	if previous, err := w.secrets.Get(ctx, ref+"#"+secrets.StagePrevious); err == nil {
		out = append(out, valuesOf(previous)...)
	}
	return out, nil
}

// valuesOf flattens material into its values.
//
// Every field of a signing secret is a candidate key: a gateway that issues two (a primary and a
// standby, as several do) stores both under one reference, and a verifier must try each. The field
// *names* are not meaningful to the verifier, only the values are.
func valuesOf(m ports.SecretMaterial) []string {
	fields := m.Fields()
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if v, ok := m.Value(f); ok && v != "" {
			out = append(out, v)
		}
	}
	return out
}

// NewWebhookIngress assembles the ≤50 ms accept path.
//
// It is here rather than in each composition root for the reason NewPaymentStack is: two binaries
// mount this surface — the dedicated ingress and, in a single-process local stack, the API — and
// they must verify signatures the same way. An ingress that verified and one that did not would
// be indistinguishable from the outside and catastrophic in exactly one direction.
func NewWebhookIngress(uow ports.UnitOfWork, rec ports.WebhookRecorder, reg *registry.Registry,
	p ports.SecretsProvider, env shared.Environment, clock shared.Clock) (*appwebhook.Ingester, error) {
	if uow == nil || rec == nil || reg == nil || p == nil {
		// Named rather than tolerated. Accepting webhooks without verifying their signatures
		// would be strictly worse than not accepting them: it lets anyone who can reach the
		// endpoint assert that a payment succeeded. The recorder is in the same sentence because
		// a verified delivery the platform cannot store is a payment outcome it never learns,
		// which turns a resolvable PROCESSING into a reconciliation exception.
		return nil, apierror.New(apierror.CodeInternalError,
			"the webhook ingress requires a unit of work, a recorder, a gateway registry and a "+
				"secrets provider; without all four it cannot accept a delivery safely")
	}
	if clock == nil {
		clock = shared.SystemClock{}
	}
	return appwebhook.NewIngester(appwebhook.IngestDeps{
		UoW: uow,
		// The accept path writes through the recorder rather than the unit of work: a delivery
		// arrives before its tenant is knowable, and the unit of work will not open a transaction
		// without a tenant. See ports.WebhookRecorder for why that guard is worth an exception
		// rather than a relaxation.
		Recorder:  rec,
		Verifiers: NewWebhookVerifiers(reg),
		Secrets:   NewWebhookSecrets(p, env),
		// Queue is nil: enqueueing is best-effort by design — the durable record is the database
		// row and the claim-unprocessed sweep is the guarantee — so a deployment without a queue
		// is a slower deployment, not a broken one.
		Clock: clock,
	}), nil
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: FR-40, NFR-32.
//
// Inbound webhook signature verification wired to the secrets provider, with both the current and
// the previous signing secret offered so a gateway's own rotation window does not become an
// endpoint outage
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
