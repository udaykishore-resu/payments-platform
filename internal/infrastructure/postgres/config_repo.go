package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/config"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// ConfigRepository persists versioned merchant configuration.
//
// Writes are append-only. A rollback publishes the previous document as a *new* version rather
// than deleting or editing the intervening ones, so the history is never destroyed — which
// matters because the first question in every configuration incident is "what were we running
// when that payment failed", and a history that has been tidied cannot answer it.
//
// The whole document is stored as JSONB rather than decomposed into columns. That is the right
// call for an open-world document that gains fields on a different cadence from the schema, and
// it is bounded by the rule stated in 04-domain-model.md §6.0: JSONB is never used for anything
// an invariant depends on. Nothing here is.
type ConfigRepository struct {
	q      querier
	tenant shared.TenantID
}

var _ ports.ConfigurationRepository = (*ConfigRepository)(nil)

const selectConfigVersion = `
SELECT tenant_id, merchant_id, environment, version, status, document,
       document_checksum, previous_version, comment, created_by, created_at, published_at
FROM pp.configuration_versions`

// GetActive returns the configuration currently in force for a merchant.
func (r *ConfigRepository) GetActive(ctx context.Context, m shared.MerchantID) (*config.MerchantConfig, error) {
	if err := requireTenantCtx(ctx, r.tenant); err != nil {
		return nil, err
	}
	c, err := scanConfig(r.q.QueryRow(ctx, selectConfigVersion+`
WHERE tenant_id = $1 AND merchant_id = $2 AND status = 'ACTIVE'`,
		r.tenant.String(), m.String()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, notFound(apierror.CodeConfigurationInvalid,
				"active configuration for merchant", m.String())
		}
		return nil, mapError(err, "get active configuration")
	}
	return c, nil
}

// GetVersion returns one specific historical version.
func (r *ConfigRepository) GetVersion(
	ctx context.Context, m shared.MerchantID, version int,
) (*config.MerchantConfig, error) {
	if err := requireTenantCtx(ctx, r.tenant); err != nil {
		return nil, err
	}
	c, err := scanConfig(r.q.QueryRow(ctx, selectConfigVersion+`
WHERE tenant_id = $1 AND merchant_id = $2 AND version = $3`,
		r.tenant.String(), m.String(), version))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, notFound(apierror.CodeConfigurationInvalid, "configuration version",
				m.String())
		}
		return nil, mapError(err, "get configuration version")
	}
	return c, nil
}

// ListVersions returns the version history newest first, cursor-paginated.
func (r *ConfigRepository) ListVersions(
	ctx context.Context, m shared.MerchantID, page ports.Page,
) ([]*config.MerchantConfig, string, error) {
	if err := requireTenantCtx(ctx, r.tenant); err != nil {
		return nil, "", err
	}
	cur, err := DecodeCursor(page.Cursor)
	if err != nil {
		return nil, "", err
	}
	limit := pageLimit(page.Limit)

	c := newCond(r.tenant.String(), m.String())
	c.raw("tenant_id = $1").raw("merchant_id = $2")
	c.keysetBefore("created_at", "configuration_version_id", cur)

	q := selectConfigVersion + c.where() +
		" ORDER BY created_at DESC, configuration_version_id DESC LIMIT " + c.limitPlaceholder()

	rows, err := r.q.Query(ctx, q, c.argsWith(limit+1)...)
	if err != nil {
		return nil, "", mapError(err, "list configuration versions")
	}
	defer rows.Close()

	out := make([]*config.MerchantConfig, 0, limit)
	for rows.Next() {
		cfg, err := scanConfig(rows)
		if err != nil {
			return nil, "", mapError(err, "list configuration versions")
		}
		out = append(out, cfg)
	}
	if err := rows.Err(); err != nil {
		return nil, "", mapError(err, "list configuration versions")
	}

	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = EncodeCursor(Cursor{Time: last.CreatedAt, ID: last.MerchantID.String()})
	}
	return out, next, nil
}

// Publish stores a new version and makes it the active one.
//
// expectedVersion implements the If-Match/ETag contract: a mismatch is a 412, not a silent
// overwrite of the edit somebody else made while this one was being composed. Two mechanisms
// enforce it and they catch different races — the `current_version = $expected` predicate on the
// head row catches the ordinary case, and the partial unique index on
// `(tenant, merchant, environment) WHERE status = 'ACTIVE'` catches two publishes that both read
// the same head before either wrote.
//
// # The checksum is computed by the database, over what the database will actually store
//
// The obvious implementation — hash the Go-marshalled bytes and send both — is wrong, and wrong
// in a way that only shows up on the read path. The column is JSONB, so PostgreSQL parses the
// document and re-serialises it in its own canonical form: key order, whitespace and duplicate
// keys are all normalised. The bytes that come back are therefore never the bytes that went in,
// the recomputed digest never matches, and *every* configuration reads as corrupt — which
// presents as a data-plane that cannot warm its snapshot and a fleet that never becomes ready.
//
// Computing it as `sha256($doc::jsonb::text)` makes both sides hash the same representation: the
// stored one. The checksum still does its job — it detects a row edited outside this code path —
// and it now detects it rather than reporting it on every read.
//
// It is computed by the database rather than trusted from the caller for the original reason too:
// a caller-supplied checksum that is never verified is a checksum that documents nothing.
func (r *ConfigRepository) Publish(
	ctx context.Context, c *config.MerchantConfig, expectedVersion int,
) error {
	if err := requireOwner(ctx, r.tenant, c.TenantID); err != nil {
		return err
	}
	doc, err := json.Marshal(c)
	if err != nil {
		return apierror.Wrap(err, apierror.CodeInternalError, "postgres: encode configuration document")
	}

	// Head row. ON CONFLICT ... WHERE current_version = expected is the optimistic guard.
	const headQ = `
INSERT INTO pp.configurations (
    configuration_id, tenant_id, merchant_id, environment, current_version, status,
    version, created_at, updated_at)
VALUES ($1,$2,$3,$4,$5,'ACTIVE',1,$6,$6)
ON CONFLICT (tenant_id, merchant_id, environment) DO UPDATE SET
    current_version = EXCLUDED.current_version,
    status          = 'ACTIVE',
    version         = pp.configurations.version + 1,
    updated_at      = EXCLUDED.updated_at
WHERE pp.configurations.current_version = $7
RETURNING configuration_id`

	configID := configurationID(c.TenantID, c.MerchantID, c.Environment)
	now := c.CreatedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}

	var returnedID string
	err = r.q.QueryRow(ctx, headQ,
		configID, c.TenantID.String(), c.MerchantID.String(), string(c.Environment),
		c.Version, now, expectedVersion).Scan(&returnedID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apierror.Newf(apierror.CodeConfigurationVersionConflict,
				"configuration for merchant %s is at a different version than expected (%d)",
				c.MerchantID, expectedVersion)
		}
		return mapError(err, "publish configuration head")
	}

	// Supersede the outgoing active version before inserting the new one, or the partial unique
	// index on ACTIVE would reject the insert. Order matters here and it is the reverse of what
	// reads naturally.
	if _, err := r.q.Exec(ctx, `
UPDATE pp.configuration_versions
SET status = 'SUPERSEDED'
WHERE tenant_id = $1 AND merchant_id = $2 AND environment = $3 AND status = 'ACTIVE'`,
		c.TenantID.String(), c.MerchantID.String(), string(c.Environment)); err != nil {
		return mapError(err, "supersede configuration version")
	}

	const versionQ = `
INSERT INTO pp.configuration_versions (
    configuration_version_id, configuration_id, tenant_id, merchant_id, environment,
    version, status, document, document_checksum, previous_version, comment,
    created_by, created_at, published_at)
VALUES ($1,$2,$3,$4,$5,$6,'ACTIVE',$7::jsonb,
        encode(sha256(convert_to($7::jsonb::text, 'UTF8')), 'hex'),
        $8,$9,$10,$11,$12)`

	published := c.PublishedAt
	if published == nil {
		published = &now
	}
	if _, err := r.q.Exec(ctx, versionQ,
		shared.NewConfigVerID().String(), returnedID, c.TenantID.String(), c.MerchantID.String(),
		string(c.Environment), c.Version, doc, c.PreviousVersion, c.Comment,
		c.CreatedBy, now, published,
	); err != nil {
		return mapError(err, "publish configuration version")
	}
	return nil
}

// ListActiveSince returns configurations published after a watermark.
//
// This is how a data-plane replica warms and refreshes its snapshot: it asks for everything
// published since the last thing it saw, rather than re-reading every merchant's configuration
// on a timer. At fifty thousand merchants the difference is the whole design of the fail-static
// cache in baseline §15.
func (r *ConfigRepository) ListActiveSince(
	ctx context.Context, since time.Time, limit int,
) ([]*config.MerchantConfig, error) {
	if err := requireTenantCtx(ctx, r.tenant); err != nil {
		return nil, err
	}
	rows, err := r.q.Query(ctx, selectConfigVersion+`
WHERE tenant_id = $1 AND status = 'ACTIVE' AND published_at > $2
ORDER BY published_at ASC
LIMIT $3`, r.tenant.String(), since.UTC(), pageLimit(limit))
	if err != nil {
		return nil, mapError(err, "list configurations since")
	}
	defer rows.Close()
	var out []*config.MerchantConfig
	for rows.Next() {
		c, err := scanConfig(rows)
		if err != nil {
			return nil, mapError(err, "list configurations since")
		}
		out = append(out, c)
	}
	return out, mapError(rows.Err(), "list configurations since")
}

// scanConfig rebuilds a MerchantConfig from its stored document.
//
// The document is the authority for the body and the columns are the authority for the identity
// and the lifecycle. Where they overlap — merchant, tenant, version, status — the columns win,
// because they are what the indexes and the constraints were applied to, and a document whose
// embedded version disagrees with its row is corrupt in a way that must not be papered over.
func scanConfig(row scanRow) (*config.MerchantConfig, error) {
	var (
		tenantID, merchantID, env string
		version                   int
		status                    string
		doc                       []byte
		checksum                  string
		prevVersion               int
		comment, createdBy        string
		createdAt                 time.Time
		publishedAt               *time.Time
	)
	if err := row.Scan(&tenantID, &merchantID, &env, &version, &status, &doc,
		&checksum, &prevVersion, &comment, &createdBy, &createdAt, &publishedAt); err != nil {
		return nil, err
	}

	var c config.MerchantConfig
	if err := json.Unmarshal(doc, &c); err != nil {
		return nil, apierror.Wrapf(err, apierror.CodeInternalError,
			"configuration %s v%d is unreadable", merchantID, version)
	}

	// Verify what was stored still hashes to what was recorded. A silent mismatch here is
	// configuration corruption, and configuration corruption resolves as payments routed by
	// rules nobody wrote.
	sum := sha256.Sum256(doc)
	if hex.EncodeToString(sum[:]) != checksum {
		return nil, apierror.Newf(apierror.CodeConfigurationInvalid,
			"configuration %s v%d does not match its recorded checksum", merchantID, version)
	}

	c.TenantID = shared.TenantID(tenantID)
	c.MerchantID = shared.MerchantID(merchantID)
	c.Environment = shared.Environment(env)
	c.Version = version
	c.Status = config.Status(status)
	c.PreviousVersion = prevVersion
	c.Comment = comment
	c.CreatedBy = createdBy
	c.CreatedAt = createdAt
	c.PublishedAt = publishedAt
	return &c, nil
}

// configurationID derives the stable head identifier from the scope it describes.
//
// Deriving it rather than minting a ULID means the head row's identity is a pure function of
// (tenant, merchant, environment), so a Publish that races another Publish for the same merchant
// targets the same row and contends on it — which is the intended behaviour — instead of
// inserting a second head that the unique index would then reject with a less useful error.
func configurationID(t shared.TenantID, m shared.MerchantID, env shared.Environment) string {
	sum := sha256.Sum256([]byte(t.String() + "|" + m.String() + "|" + string(env)))
	return "cfg_" + hex.EncodeToString(sum[:16])
}

// ConfigSnapshotReader is the data plane's platform-wide read of published configuration.
//
// # Why it is not the tenant-scoped repository
//
// ConfigRepository.ListActiveSince reads *one tenant's* configurations, which is right for a
// control-plane caller acting inside a tenant's request. The data plane's snapshot is the other
// shape entirely: one pod serves every tenant that routes to it, so it must warm with every
// tenant's active configuration, and it does so at startup — before any request and therefore
// before any tenant context exists.
//
// Forcing a tenant here would mean either inventing one at startup or looping over the tenant
// table and issuing a query per tenant, which for a large estate is thousands of round trips on
// the path that must complete before a pod becomes ready. Reading the whole active set in one
// watermark-bounded statement is what makes the fail-static snapshot (baseline §15) affordable.
//
// The reduced blast radius is deliberate: this type can read published configuration and nothing
// else, and it cannot write.
type ConfigSnapshotReader struct {
	pool *Pool
}

// NewConfigSnapshotReader builds the reader.
func NewConfigSnapshotReader(pool *Pool) *ConfigSnapshotReader {
	return &ConfigSnapshotReader{pool: pool}
}

// Load returns every ACTIVE configuration published after the watermark, across every tenant.
//
// Ordered by publication time so the caller's watermark advances monotonically: a page ordered
// any other way would either re-read rows forever or skip one published in the same millisecond
// as the page boundary.
func (r *ConfigSnapshotReader) Load(ctx context.Context, since time.Time, limit int) ([]*config.MerchantConfig, error) {
	rows, err := r.pool.pool.Query(ctx, selectConfigVersion+`
WHERE status = 'ACTIVE' AND published_at > $1
ORDER BY published_at ASC
LIMIT $2`, since.UTC(), pageLimit(limit))
	if err != nil {
		return nil, mapError(err, "list active configurations")
	}
	defer rows.Close()
	var out []*config.MerchantConfig
	for rows.Next() {
		c, err := scanConfig(rows)
		if err != nil {
			return nil, mapError(err, "list active configurations")
		}
		out = append(out, c)
	}
	return out, mapError(rows.Err(), "list active configurations")
}
