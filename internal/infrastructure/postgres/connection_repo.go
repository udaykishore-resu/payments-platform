package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/gateway"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// ConnectionRepository persists the binding between one merchant and one gateway in one
// environment.
//
// What this table holds about credentials is a *path* into the secrets manager, never material.
// That is not a convention here — it is a column type and a CHECK constraint, because a
// convention is what gets broken by the one person who needed to unblock a test at 2 a.m.
type ConnectionRepository struct {
	q      querier
	tenant shared.TenantID
}

var _ ports.ConnectionRepository = (*ConnectionRepository)(nil)

const selectConnection = `
SELECT connection_id, tenant_id, merchant_id, gateway_id, environment,
       status, certification_status, certification_report_id,
       external_account_ref, credential_ref, webhook_registration_id, webhook_endpoint,
       status_reason, revocation_reason, version,
       created_at, updated_at, provisioned_at, certified_at, revoked_at, last_health_check_at
FROM pp.gateway_connections`

// Get loads one connection by identifier.
func (r *ConnectionRepository) Get(ctx context.Context, id shared.ConnectionID) (*gateway.Connection, error) {
	return r.one(ctx, id.String(),
		selectConnection+" WHERE tenant_id = $1 AND connection_id = $2",
		r.tenant.String(), id.String())
}

// GetByMerchantGateway resolves the connection the dispatcher needs before a gateway call.
//
// The environment is not a parameter because a merchant exists in exactly one environment
// (shared.Environment is a property of the merchant, not of the request), and accepting it here
// would create a code path in which a production payment could be dispatched against a sandbox
// connection — the failure mode where a certification run charges a real card, inverted.
func (r *ConnectionRepository) GetByMerchantGateway(
	ctx context.Context, m shared.MerchantID, g shared.GatewayID,
) (*gateway.Connection, error) {
	return r.one(ctx, m.String()+"/"+g.String(),
		selectConnection+`
WHERE tenant_id = $1 AND merchant_id = $2 AND gateway_id = $3
ORDER BY created_at DESC
LIMIT 1`, r.tenant.String(), m.String(), g.String())
}

func (r *ConnectionRepository) one(ctx context.Context, subject, q string, args ...any) (*gateway.Connection, error) {
	if err := requireTenantCtx(ctx, r.tenant); err != nil {
		return nil, err
	}
	c, err := scanConnection(r.q.QueryRow(ctx, q, args...))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, notFound(apierror.CodeGatewayNotConfigured, "gateway connection", subject)
		}
		return nil, mapError(err, "get gateway connection")
	}
	return c, nil
}

// ListForMerchant returns every connection a merchant has, in any state.
//
// Revoked connections are included: the activation guard counts CERTIFIED ones, and an operator
// answering "why can this merchant not take payments" needs to see the revoked one that used to
// work far more than they need a tidy list.
func (r *ConnectionRepository) ListForMerchant(
	ctx context.Context, m shared.MerchantID,
) ([]*gateway.Connection, error) {
	if err := requireTenantCtx(ctx, r.tenant); err != nil {
		return nil, err
	}
	rows, err := r.q.Query(ctx, selectConnection+`
WHERE tenant_id = $1 AND merchant_id = $2
ORDER BY gateway_id`, r.tenant.String(), m.String())
	if err != nil {
		return nil, mapError(err, "list gateway connections")
	}
	defer rows.Close()
	var out []*gateway.Connection
	for rows.Next() {
		c, err := scanConnection(rows)
		if err != nil {
			return nil, mapError(err, "list gateway connections")
		}
		out = append(out, c)
	}
	return out, mapError(rows.Err(), "list gateway connections")
}

// Save upserts a connection under optimistic concurrency.
func (r *ConnectionRepository) Save(ctx context.Context, c *gateway.Connection) error {
	if err := requireOwner(ctx, r.tenant, c.TenantID()); err != nil {
		return err
	}
	const q = `
INSERT INTO pp.gateway_connections (
    connection_id, tenant_id, merchant_id, gateway_id, environment,
    status, certification_status, certification_report_id,
    external_account_ref, credential_ref, webhook_registration_id, webhook_endpoint,
    status_reason, revocation_reason, version,
    created_at, updated_at, provisioned_at, certified_at, revoked_at, last_health_check_at,
    credential_rotated_at, credential_expires_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)
ON CONFLICT (connection_id) DO UPDATE SET
    status = EXCLUDED.status,
    certification_status = EXCLUDED.certification_status,
    certification_report_id = EXCLUDED.certification_report_id,
    external_account_ref = EXCLUDED.external_account_ref,
    credential_ref = EXCLUDED.credential_ref,
    webhook_registration_id = EXCLUDED.webhook_registration_id,
    webhook_endpoint = EXCLUDED.webhook_endpoint,
    status_reason = EXCLUDED.status_reason,
    revocation_reason = EXCLUDED.revocation_reason,
    version = EXCLUDED.version,
    updated_at = EXCLUDED.updated_at,
    provisioned_at = EXCLUDED.provisioned_at,
    certified_at = EXCLUDED.certified_at,
    revoked_at = EXCLUDED.revoked_at,
    last_health_check_at = EXCLUDED.last_health_check_at,
    credential_rotated_at = EXCLUDED.credential_rotated_at,
    credential_expires_at = EXCLUDED.credential_expires_at
WHERE pp.gateway_connections.version = EXCLUDED.version - 1`

	// The credential's age is tracked from the last status change that touched it. The 90-day
	// ceiling is a CHECK constraint on the table, so a rotation that sets an expiry beyond it is
	// refused by the database rather than by a job that might not run.
	var rotatedAt, expiresAt *time.Time
	if c.CredentialRef() != "" {
		t := c.UpdatedAt()
		rotatedAt = &t
		e := t.Add(90 * 24 * time.Hour)
		expiresAt = &e
	}

	tag, err := r.q.Exec(ctx, q,
		c.ID().String(), c.TenantID().String(), c.MerchantID().String(), c.GatewayID().String(),
		string(c.Environment()), string(c.Status()), string(c.CertificationStatus()),
		c.CertificationReportID(), c.ExternalAccountRef(), c.CredentialRef(),
		c.WebhookRegistrationID(), c.WebhookEndpoint(),
		c.StatusReason(), c.RevocationReason(), int64(c.Version()),
		c.CreatedAt(), c.UpdatedAt(), c.ProvisionedAt(), c.CertifiedAt(), c.RevokedAt(),
		c.LastHealthCheckAt(), rotatedAt, expiresAt,
	)
	if err != nil {
		return mapError(err, "save gateway connection")
	}
	if tag.RowsAffected() == 0 {
		return apierror.Newf(apierror.CodeConfigurationVersionConflict,
			"gateway connection %s was modified concurrently; reload and reapply", c.ID())
	}
	return nil
}

// FindCredentialsDueForRotation returns connections whose credentials exceed the maximum age.
//
// It drives the automated rotation workflow, and it is a query rather than a calendar because
// credentials are rotated at different times for different reasons — provisioning, an incident,
// a manual rotation — so "everything ninety days after it was last touched" is the only correct
// selection.
func (r *ConnectionRepository) FindCredentialsDueForRotation(
	ctx context.Context, olderThan time.Duration, limit int,
) ([]*gateway.Connection, error) {
	if err := requireTenantCtx(ctx, r.tenant); err != nil {
		return nil, err
	}
	rows, err := r.q.Query(ctx, selectConnection+`
WHERE tenant_id = $1
  AND status <> 'REVOKED'
  AND credential_rotated_at IS NOT NULL
  AND credential_rotated_at < $2
ORDER BY credential_rotated_at ASC
LIMIT $3`, r.tenant.String(), time.Now().UTC().Add(-olderThan), pageLimit(limit))
	if err != nil {
		return nil, mapError(err, "find rotatable credentials")
	}
	defer rows.Close()
	var out []*gateway.Connection
	for rows.Next() {
		c, err := scanConnection(rows)
		if err != nil {
			return nil, mapError(err, "find rotatable credentials")
		}
		out = append(out, c)
	}
	return out, mapError(rows.Err(), "find rotatable credentials")
}

func scanConnection(row scanRow) (*gateway.Connection, error) {
	var (
		p                          gateway.RehydrateConnectionParams
		id, tenant, merch, gw, env string
		status, certStatus         string
		version                    int64
	)
	if err := row.Scan(&id, &tenant, &merch, &gw, &env,
		&status, &certStatus, &p.CertificationReportID,
		&p.ExternalAccountRef, &p.CredentialRef, &p.WebhookRegistrationID, &p.WebhookEndpoint,
		&p.StatusReason, &p.RevocationReason, &version,
		&p.CreatedAt, &p.UpdatedAt, &p.ProvisionedAt, &p.CertifiedAt, &p.RevokedAt,
		&p.LastHealthCheckAt); err != nil {
		return nil, err
	}
	p.ID = shared.ConnectionID(id)
	p.TenantID = shared.TenantID(tenant)
	p.MerchantID = shared.MerchantID(merch)
	p.GatewayID = shared.GatewayID(gw)
	p.Environment = shared.Environment(env)
	p.Status = gateway.ConnectionStatus(status)
	p.CertificationStatus = gateway.CertificationStatus(certStatus)
	p.Version = shared.Version(version)
	return gateway.RehydrateConnection(p)
}
