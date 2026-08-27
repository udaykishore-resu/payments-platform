package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/domain/tenant"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// TenantRepository persists tenants and their API clients.
//
// The tenants table is under the same RLS policy as everything else, comparing its own primary
// key against the GUC. That looks redundant — a tenant reading its own row — and it is not: it
// is what stops a compromised or buggy control-plane path from enumerating the tenant list, and
// the tenant list is the most useful thing an attacker inside the platform could obtain.
type TenantRepository struct {
	q      querier
	tenant shared.TenantID
}

var _ ports.TenantRepository = (*TenantRepository)(nil)

const selectTenant = `
SELECT tenant_id, name, tier, status, residency_region, kms_key_ref,
       environments, enabled_gateways, enabled_currencies, enabled_methods, feature_flags,
       max_merchants, requests_per_second, concurrent_payments, cache_memory_mb,
       max_payment_amount, max_payment_currency,
       status_reason, version, created_at, updated_at, suspended_at, terminated_at
FROM pp.tenants`

// Get loads a tenant. The identifier must be the caller's own tenant: RLS would return zero rows
// for any other, and asking for one is a bug worth reporting rather than a 404 worth returning.
func (r *TenantRepository) Get(ctx context.Context, id shared.TenantID) (*tenant.Tenant, error) {
	if err := requireTenantCtx(ctx, r.tenant); err != nil {
		return nil, err
	}
	var (
		p                            tenant.RehydrateParams
		tid, tier, status, residency string
		// kmsRef is a pointer because the column is nullable and legitimately NULL for a pooled
		// tenant: the per-tenant CMK is a siloed-tier feature, and the table's own CHECK requires
		// the reference only for SILOED. Scanning it into a string made every pooled tenant
		// unreadable — a 500 on the routing path with the message "get tenant failed", which
		// names neither the column nor the tier.
		kmsRef                       *string
		envs, gws, ccys, methods     []string
		flagsRaw                     []byte
		maxMerch, rps, conc, cacheMB int
		maxAmt                       int64
		maxCcy                       *string
		version                      int64
	)
	err := r.q.QueryRow(ctx, selectTenant+" WHERE tenant_id = $1", id.String()).Scan(
		&tid, &p.Name, &tier, &status, &residency, &kmsRef,
		&envs, &gws, &ccys, &methods, &flagsRaw,
		&maxMerch, &rps, &conc, &cacheMB, &maxAmt, &maxCcy,
		&p.StatusReason, &version, &p.CreatedAt, &p.UpdatedAt, &p.SuspendedAt, &p.TerminatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, notFound(apierror.CodeForbidden, "tenant", id.String())
		}
		return nil, mapError(err, "get tenant")
	}

	p.ID = shared.TenantID(tid)
	p.Tier = shared.TenantTier(tier)
	p.Status = tenant.Status(status)
	p.Residency = tenant.ResidencyRegion(residency)
	if kmsRef != nil {
		p.KMSKeyRef = *kmsRef
	}
	p.Version = shared.Version(version)
	for _, e := range envs {
		p.Environments = append(p.Environments, shared.Environment(e))
	}
	for _, g := range gws {
		p.EnabledGateways = append(p.EnabledGateways, shared.GatewayID(g))
	}
	for _, c := range ccys {
		p.EnabledCurrencies = append(p.EnabledCurrencies, money.Currency(c))
	}
	for _, m := range methods {
		p.EnabledMethods = append(p.EnabledMethods, shared.PaymentMethod(m))
	}
	if len(flagsRaw) > 0 {
		if err := json.Unmarshal(flagsRaw, &p.FeatureFlags); err != nil {
			return nil, apierror.Wrapf(err, apierror.CodeInternalError,
				"tenant %s has unreadable feature flags", tid)
		}
	}
	p.Quotas = tenant.Quotas{
		MaxMerchants:       maxMerch,
		RequestsPerSecond:  rps,
		ConcurrentPayments: conc,
		CacheMemoryMB:      cacheMB,
	}
	if maxCcy != nil && maxAmt > 0 {
		p.Quotas.MaxPaymentAmount = safeMoney(maxAmt, *maxCcy)
	}
	return tenant.Rehydrate(p)
}

// Save persists a tenant under optimistic concurrency.
//
// tier and residency_region are absent from the SET list. They are immutable — a pooled-to-siloed
// move is the online migration in docs/multi-tenancy.md §5.1, and an UPDATE that merely relabels
// the tier leaves every row in the pooled schema while the rest of the platform believes the
// tenant is isolated. The trigger in migration 0013 refuses the change for the paths that do not
// come through here.
func (r *TenantRepository) Save(ctx context.Context, t *tenant.Tenant) error {
	if err := requireTenantCtx(ctx, r.tenant); err != nil {
		return err
	}
	const q = `
UPDATE pp.tenants SET
    name                = $3,
    status              = $4,
    kms_key_ref         = $5,
    environments        = $6,
    enabled_gateways    = $7,
    enabled_currencies  = $8,
    enabled_methods     = $9,
    feature_flags       = $10,
    max_merchants       = $11,
    requests_per_second = $12,
    concurrent_payments = $13,
    cache_memory_mb     = $14,
    max_payment_amount  = $15,
    max_payment_currency = $16,
    status_reason       = $17,
    version             = $18,
    updated_at          = $19,
    suspended_at        = $20,
    terminated_at       = $21
WHERE tenant_id = $1 AND version = $2`

	flags, err := json.Marshal(t.FeatureFlags())
	if err != nil {
		return apierror.Wrap(err, apierror.CodeInternalError, "postgres: encode tenant feature flags")
	}
	q4 := t.Quotas()

	tag, err := r.q.Exec(ctx, q,
		t.ID().String(), int64(t.Version())-1,
		t.Name(), string(t.Status()), t.KMSKeyRef(),
		stringsOf(t.Environments()), stringsOf(t.EnabledGateways()),
		stringsOf(t.EnabledCurrencies()), stringsOf(t.EnabledMethods()), flags,
		q4.MaxMerchants, q4.RequestsPerSecond, q4.ConcurrentPayments, q4.CacheMemoryMB,
		q4.MaxPaymentAmount.Amount(), nullIfEmpty(string(q4.MaxPaymentAmount.Currency())),
		t.StatusReason(), int64(t.Version()), t.UpdatedAt(), t.SuspendedAt(), t.TerminatedAt(),
	)
	if err != nil {
		return mapError(err, "save tenant")
	}
	if tag.RowsAffected() == 0 {
		return apierror.Newf(apierror.CodeConfigurationVersionConflict,
			"tenant %s was modified concurrently; reload and reapply", t.ID())
	}
	return nil
}

// GetAPIClient loads an API client for authentication.
//
// It returns UNAUTHENTICATED for a missing client rather than a not-found, because this call
// sits on the authentication path and the caller is by definition not yet trusted: telling an
// unauthenticated caller that a client identifier does not exist lets them enumerate which ones
// do.
func (r *TenantRepository) GetAPIClient(ctx context.Context, id shared.APIClientID) (*tenant.APIClient, error) {
	if err := requireTenantCtx(ctx, r.tenant); err != nil {
		return nil, err
	}
	const q = `
SELECT client_id, tenant_id, name, scopes, allowed_cidrs, credential_ref,
       previous_credential_ref, rotation_overlap_until, status, status_reason,
       created_at, updated_at, last_rotated_at, revoked_at, version
FROM pp.api_clients
WHERE client_id = $1 AND tenant_id = $2`

	var (
		p                tenant.RehydrateAPIClientParams
		cid, tid, status string
		overlapUntil     *time.Time
		version          int64
	)
	err := r.q.QueryRow(ctx, q, id.String(), r.tenant.String()).Scan(
		&cid, &tid, &p.Name, &p.Scopes, &p.AllowedCIDRs, &p.CredentialRef,
		&p.PreviousCredentialRef, &overlapUntil, &status, &p.StatusReason,
		&p.CreatedAt, &p.UpdatedAt, &p.LastRotatedAt, &p.RevokedAt, &version,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apierror.New(apierror.CodeUnauthenticated, "unknown API client")
		}
		return nil, mapError(err, "get api client")
	}
	p.ID = shared.APIClientID(cid)
	p.TenantID = shared.TenantID(tid)
	p.Status = tenant.ClientStatus(status)
	p.Version = shared.Version(version)
	if overlapUntil != nil {
		p.RotationOverlapUntil = *overlapUntil
	}
	return tenant.RehydrateAPIClient(p)
}

// stringsOf converts a slice of any string-kinded domain type to []string for a TEXT[] bind.
//
// Generic rather than five near-identical loops: five loops is five places for a copy-paste that
// converts the wrong slice, and the compiler cannot see the mistake because every one of these
// types is a string underneath.
func stringsOf[T ~string](in []T) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = string(v)
	}
	return out
}
