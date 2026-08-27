package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/merchant"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// MerchantRepository persists the Merchant aggregate: the merchant row plus its business
// profile, bank accounts, principals and attestations, written together under one
// merchants.version check.
//
// The four child tables are loaded in the same round trip as the parent, by the same JSONB
// aggregation the payment repository uses. Cardinality is bounded by the aggregate's own
// invariants — at most twenty bank accounts, at most twenty-five principals — so loading the
// whole aggregate is a fixed cost rather than an unbounded one, which is exactly why those
// bounds exist.
type MerchantRepository struct {
	q      querier
	tenant shared.TenantID
	clock  shared.Clock
}

var _ ports.MerchantRepository = (*MerchantRepository)(nil)

const selectMerchant = `
SELECT m.merchant_id, m.tenant_id, m.external_reference, m.legal_name, m.display_name,
       m.environment, m.status, m.status_reason, m.suspension_reason,
       m.kyc_status, m.kyc_provider_ref, m.kyc_completed_at, m.kyc_expires_at, m.risk_rating,
       m.certification_id, m.active_config_version, m.version,
       m.created_at, m.updated_at, m.activated_at, m.suspended_at,
       coalesce(bp.legal_entity_type, ''), coalesce(bp.registration_number, ''),
       coalesce(bp.tax_id_ref, ''), coalesce(bp.tax_id_last4, ''), bp.incorporation_date,
       coalesce(bp.country, ''), coalesce(bp.address_line1, ''), coalesce(bp.address_line2, ''),
       coalesce(bp.city, ''), coalesce(bp.region, ''), coalesce(bp.postal_code, ''),
       coalesce(bp.website_url, ''), coalesce(bp.support_email, ''), coalesce(bp.support_phone, ''),
       coalesce(bp.mcc, ''), coalesce(bp.description, ''),
       coalesce(bp.expected_monthly_volume, 0), coalesce(bp.expected_monthly_volume_ccy, 'USD'),
       coalesce(bp.expected_average_ticket, 0), coalesce(bp.expected_average_ticket_ccy, 'USD'),
       coalesce((
           SELECT jsonb_agg(jsonb_build_object(
               'id', b.bank_account_id, 'country', b.country, 'currency', b.currency,
               'holder', b.holder_name, 'account_last4', b.account_last4,
               'routing_last4', b.routing_last4, 'iban_last4', b.iban_last4,
               'secret_ref', b.secret_ref, 'status', b.status,
               'validation_ref', b.validation_ref,
               'validated_at', (EXTRACT(EPOCH FROM b.validated_at) * 1000000)::bigint,
               'is_default', b.is_default, 'failure_reason', b.failure_reason
           ) ORDER BY b.sort_order, b.bank_account_id)
           FROM pp.merchant_bank_accounts b
           WHERE b.tenant_id = m.tenant_id AND b.merchant_id = m.merchant_id
       ), '[]'::jsonb),
       coalesce((
           SELECT jsonb_agg(jsonb_build_object(
               'id', pr.principal_id, 'role', pr.role, 'first_name', pr.first_name,
               'last_name', pr.last_name, 'ownership_pct', pr.ownership_pct,
               'country', pr.country, 'verification_ref', pr.verification_ref,
               'verified', pr.verified
           ) ORDER BY pr.sort_order, pr.principal_id)
           FROM pp.merchant_principals pr
           WHERE pr.tenant_id = m.tenant_id AND pr.merchant_id = m.merchant_id
       ), '[]'::jsonb),
       coalesce((
           SELECT jsonb_agg(jsonb_build_object(
               'type', at.type, 'reference', at.reference, 'attested_by', at.attested_by,
               'attested_at', (EXTRACT(EPOCH FROM at.attested_at) * 1000000)::bigint,
               'expires_at',  (EXTRACT(EPOCH FROM at.expires_at)  * 1000000)::bigint,
               'document_id', at.document_id
           ) ORDER BY at.type)
           FROM pp.merchant_attestations at
           WHERE at.tenant_id = m.tenant_id AND at.merchant_id = m.merchant_id
       ), '[]'::jsonb)
FROM pp.merchants m
LEFT JOIN pp.merchant_business_profile bp
       ON bp.tenant_id = m.tenant_id AND bp.merchant_id = m.merchant_id`

// Create inserts the merchant and its children.
func (r *MerchantRepository) Create(ctx context.Context, m *merchant.Merchant) error {
	if err := requireOwner(ctx, r.tenant, m.TenantID()); err != nil {
		return err
	}
	const q = `
INSERT INTO pp.merchants (
    merchant_id, tenant_id, external_reference, legal_name, display_name, environment,
    status, status_reason, suspension_reason, kyc_status, kyc_provider_ref,
    kyc_completed_at, kyc_expires_at, risk_rating, certification_id, active_config_version,
    version, created_at, updated_at, activated_at, suspended_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)`

	if _, err := r.q.Exec(ctx, q,
		m.ID().String(), m.TenantID().String(), nullIfEmpty(m.ExternalRef()),
		m.LegalName(), m.DisplayName(), string(m.Environment()),
		string(m.Status()), m.StatusReason(), string(m.SuspensionReason()),
		string(m.KYCStatus()), m.KYCProviderRef(), m.KYCCompletedAt(), m.KYCExpiresAt(),
		string(m.RiskRating()), m.CertificationID(), m.ActiveConfigVersion(),
		int64(m.Version()), m.CreatedAt(), m.UpdatedAt(), m.ActivatedAt(), m.SuspendedAt(),
	); err != nil {
		return mapError(err, "create merchant")
	}
	return r.saveChildren(ctx, m)
}

// Get loads a merchant by identifier within the caller's tenant.
func (r *MerchantRepository) Get(ctx context.Context, id shared.MerchantID) (*merchant.Merchant, error) {
	return r.one(ctx, selectMerchant+`
WHERE m.tenant_id = $1 AND m.merchant_id = $2`, id.String(), r.tenant.String(), id.String())
}

// GetForUpdate loads a merchant holding a row lock on the merchant row only.
//
// The merchant row is the concurrency point for every child mutation: adding a bank account
// bumps merchants.version, so a concurrent status transition loses and retries. Locking the
// parent is therefore sufficient, and `FOR UPDATE OF m` keeps the lock off the child rows,
// which a bare FOR UPDATE would also take for no benefit.
func (r *MerchantRepository) GetForUpdate(ctx context.Context, id shared.MerchantID) (*merchant.Merchant, error) {
	return r.one(ctx, selectMerchant+`
WHERE m.tenant_id = $1 AND m.merchant_id = $2
FOR UPDATE OF m`, id.String(), r.tenant.String(), id.String())
}

// GetByExternalRef finds a merchant by the tenant's own identifier for it.
//
// This is how a tenant's systems locate a merchant without storing our identifiers, which is
// what makes onboarding idempotent from their side: retrying a create with the same external
// reference finds the existing merchant instead of making a second one.
func (r *MerchantRepository) GetByExternalRef(ctx context.Context, ref string) (*merchant.Merchant, error) {
	return r.one(ctx, selectMerchant+`
WHERE m.tenant_id = $1 AND m.external_reference = $2`, ref, r.tenant.String(), ref)
}

func (r *MerchantRepository) one(ctx context.Context, q, subject string, args ...any) (*merchant.Merchant, error) {
	if err := requireTenantCtx(ctx, r.tenant); err != nil {
		return nil, err
	}
	m, err := scanMerchant(r.q.QueryRow(ctx, q, args...))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, notFound(apierror.CodeMerchantNotFound, "merchant", subject)
		}
		return nil, mapError(err, "get merchant")
	}
	return m, nil
}

// Save persists a modified merchant under optimistic concurrency.
func (r *MerchantRepository) Save(ctx context.Context, m *merchant.Merchant) error {
	if err := requireOwner(ctx, r.tenant, m.TenantID()); err != nil {
		return err
	}
	const q = `
UPDATE pp.merchants SET
    external_reference    = $4,
    legal_name            = $5,
    display_name          = $6,
    status                = $7,
    status_reason         = $8,
    suspension_reason     = $9,
    kyc_status            = $10,
    kyc_provider_ref      = $11,
    kyc_completed_at      = $12,
    kyc_expires_at        = $13,
    risk_rating           = $14,
    certification_id      = $15,
    active_config_version = $16,
    version               = $17,
    updated_at            = $18,
    activated_at          = $19,
    suspended_at          = $20
WHERE tenant_id = $1 AND merchant_id = $2 AND version = $3`

	tag, err := r.q.Exec(ctx, q,
		m.TenantID().String(), m.ID().String(), int64(m.Version())-1,
		nullIfEmpty(m.ExternalRef()), m.LegalName(), m.DisplayName(),
		string(m.Status()), m.StatusReason(), string(m.SuspensionReason()),
		string(m.KYCStatus()), m.KYCProviderRef(), m.KYCCompletedAt(), m.KYCExpiresAt(),
		string(m.RiskRating()), m.CertificationID(), m.ActiveConfigVersion(),
		int64(m.Version()), m.UpdatedAt(), m.ActivatedAt(), m.SuspendedAt(),
	)
	if err != nil {
		return mapError(err, "save merchant")
	}
	if tag.RowsAffected() == 0 {
		return apierror.Newf(apierror.CodeMerchantNotActive,
			"merchant %s was modified concurrently; reload and reapply", m.ID()).
			WithDetail(apierror.Detail{
				Code:    "VERSION_CONFLICT",
				Message: "another writer advanced this merchant past the version you loaded",
				RuleID:  "R-CC-1.OPTIMISTIC_CONCURRENCY",
			})
	}
	return r.saveChildren(ctx, m)
}

// saveChildren upserts the profile, bank accounts, principals and attestations.
//
// Children are upserted, never deleted-and-reinserted. A principal row deleted and recreated
// gets a new surrogate identity, which breaks the KYC vendor reference attached to it — and a
// UBO whose verification reference has silently changed is a UBO who has to be re-verified.
func (r *MerchantRepository) saveChildren(ctx context.Context, m *merchant.Merchant) error {
	p := m.Profile()
	const profileQ = `
INSERT INTO pp.merchant_business_profile (
    merchant_id, tenant_id, legal_entity_type, registration_number, tax_id_ref, tax_id_last4,
    incorporation_date, country, address_line1, address_line2, city, region, postal_code,
    website_url, support_email, support_phone, mcc, description,
    expected_monthly_volume, expected_monthly_volume_ccy,
    expected_average_ticket, expected_average_ticket_ccy)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)
ON CONFLICT (merchant_id) DO UPDATE SET
    legal_entity_type = EXCLUDED.legal_entity_type,
    registration_number = EXCLUDED.registration_number,
    tax_id_ref = EXCLUDED.tax_id_ref,
    tax_id_last4 = EXCLUDED.tax_id_last4,
    incorporation_date = EXCLUDED.incorporation_date,
    country = EXCLUDED.country,
    address_line1 = EXCLUDED.address_line1,
    address_line2 = EXCLUDED.address_line2,
    city = EXCLUDED.city,
    region = EXCLUDED.region,
    postal_code = EXCLUDED.postal_code,
    website_url = EXCLUDED.website_url,
    support_email = EXCLUDED.support_email,
    support_phone = EXCLUDED.support_phone,
    mcc = EXCLUDED.mcc,
    description = EXCLUDED.description,
    expected_monthly_volume = EXCLUDED.expected_monthly_volume,
    expected_monthly_volume_ccy = EXCLUDED.expected_monthly_volume_ccy,
    expected_average_ticket = EXCLUDED.expected_average_ticket,
    expected_average_ticket_ccy = EXCLUDED.expected_average_ticket_ccy`

	if _, err := r.q.Exec(ctx, profileQ,
		m.ID().String(), m.TenantID().String(), p.LegalEntityType, p.RegistrationNumber,
		p.TaxID, p.TaxIDLast4, p.IncorporationDate, orTwoLetter(string(p.Country)),
		p.AddressLine1, p.AddressLine2, p.City, p.Region, p.PostalCode,
		p.WebsiteURL, p.SupportEmail, p.SupportPhone, orFourDigit(string(p.MCC)), p.Description,
		p.ExpectedMonthlyVolume.Amount(), orCurrency(p.ExpectedMonthlyVolume.Currency()),
		p.ExpectedAverageTicket.Amount(), orCurrency(p.ExpectedAverageTicket.Currency()),
	); err != nil {
		return mapError(err, "save merchant profile")
	}

	const bankQ = `
INSERT INTO pp.merchant_bank_accounts (
    bank_account_id, tenant_id, merchant_id, country, currency, holder_name,
    account_last4, routing_last4, iban_last4, secret_ref, status, validation_ref,
    validated_at, is_default, failure_reason, sort_order)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
ON CONFLICT (bank_account_id) DO UPDATE SET
    status = EXCLUDED.status, validation_ref = EXCLUDED.validation_ref,
    validated_at = EXCLUDED.validated_at, is_default = EXCLUDED.is_default,
    failure_reason = EXCLUDED.failure_reason, sort_order = EXCLUDED.sort_order`

	for i, b := range m.BankAccounts() {
		if _, err := r.q.Exec(ctx, bankQ,
			b.ID, m.TenantID().String(), m.ID().String(), orTwoLetter(string(b.Country)),
			orCurrency(b.Currency), b.HolderName, b.AccountLast4, b.RoutingLast4, b.IBANLast4,
			b.SecretRef, string(b.Status), b.ValidationRef, b.ValidatedAt, b.IsDefault,
			b.FailureReason, i,
		); err != nil {
			return mapError(err, "save merchant bank account")
		}
	}

	const principalQ = `
INSERT INTO pp.merchant_principals (
    principal_id, tenant_id, merchant_id, role, first_name, last_name,
    ownership_pct, country, verification_ref, verified, sort_order)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
ON CONFLICT (principal_id) DO UPDATE SET
    role = EXCLUDED.role, first_name = EXCLUDED.first_name, last_name = EXCLUDED.last_name,
    ownership_pct = EXCLUDED.ownership_pct, country = EXCLUDED.country,
    verification_ref = EXCLUDED.verification_ref, verified = EXCLUDED.verified,
    sort_order = EXCLUDED.sort_order`

	for i, pr := range m.Principals() {
		if _, err := r.q.Exec(ctx, principalQ,
			pr.ID, m.TenantID().String(), m.ID().String(), string(pr.Role),
			pr.FirstName, pr.LastName, pr.OwnershipPct, orTwoLetter(string(pr.Country)),
			pr.VerificationRef, pr.Verified, i,
		); err != nil {
			return mapError(err, "save merchant principal")
		}
	}

	const attQ = `
INSERT INTO pp.merchant_attestations (
    tenant_id, merchant_id, type, reference, attested_by, attested_at, expires_at, document_id)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
ON CONFLICT (tenant_id, merchant_id, type) DO UPDATE SET
    reference = EXCLUDED.reference, attested_by = EXCLUDED.attested_by,
    attested_at = EXCLUDED.attested_at, expires_at = EXCLUDED.expires_at,
    document_id = EXCLUDED.document_id`

	for _, a := range m.Attestations() {
		if _, err := r.q.Exec(ctx, attQ,
			m.TenantID().String(), m.ID().String(), a.Type, a.Reference,
			a.AttestedBy, a.AttestedAt, a.ExpiresAt, a.DocumentID,
		); err != nil {
			return mapError(err, "save merchant attestation")
		}
	}
	return nil
}

// List returns a tenant-scoped, cursor-paginated page of merchants.
func (r *MerchantRepository) List(
	ctx context.Context, f ports.MerchantFilter, page ports.Page,
) ([]*merchant.Merchant, string, error) {
	if err := requireTenantCtx(ctx, r.tenant); err != nil {
		return nil, "", err
	}
	cur, err := DecodeCursor(page.Cursor)
	if err != nil {
		return nil, "", err
	}
	limit := pageLimit(page.Limit)

	c := newCond(r.tenant.String())
	c.raw("m.tenant_id = $1")
	if len(f.Statuses) > 0 {
		ss := make([]string, 0, len(f.Statuses))
		for _, s := range f.Statuses {
			ss = append(ss, string(s))
		}
		c.inStrings("m.status", ss)
	}
	if f.Country != "" {
		c.eq("bp.country", string(f.Country))
	}
	if f.Environment != "" {
		c.eq("m.environment", string(f.Environment))
	}
	// Search matches the display name by prefix. The value is escaped for LIKE metacharacters
	// so a search for "%" cannot turn into a full scan of the tenant's merchants.
	c.ilike("m.display_name", f.Search)
	c.keysetBefore("m.created_at", "m.merchant_id", cur)

	q := selectMerchant + c.where() +
		" ORDER BY m.created_at DESC, m.merchant_id DESC LIMIT " + c.limitPlaceholder()

	rows, err := r.q.Query(ctx, q, c.argsWith(limit+1)...)
	if err != nil {
		return nil, "", mapError(err, "list merchants")
	}
	defer rows.Close()

	out := make([]*merchant.Merchant, 0, limit)
	for rows.Next() {
		m, err := scanMerchant(rows)
		if err != nil {
			return nil, "", mapError(err, "list merchants")
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, "", mapError(err, "list merchants")
	}

	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = EncodeCursor(Cursor{Time: last.CreatedAt(), ID: last.ID().String()})
	}
	return out, next, nil
}

// FindKYCExpiring returns merchants whose verification lapses within the window.
//
// Re-verification has to start before processing stops, not after. A merchant who discovers
// their KYC lapsed because their payments began failing has already lost the sales made during
// the gap, and the platform has already taken the support call.
func (r *MerchantRepository) FindKYCExpiring(
	ctx context.Context, within time.Duration, limit int,
) ([]*merchant.Merchant, error) {
	if err := requireTenantCtx(ctx, r.tenant); err != nil {
		return nil, err
	}
	q := selectMerchant + `
WHERE m.tenant_id = $1
  AND m.kyc_expires_at IS NOT NULL
  AND m.kyc_expires_at <= $2
  AND m.status <> 'TERMINATED'
ORDER BY m.kyc_expires_at ASC
LIMIT $3`
	rows, err := r.q.Query(ctx, q, r.tenant.String(),
		r.clock.Now().Add(within), pageLimit(limit))
	if err != nil {
		return nil, mapError(err, "find expiring kyc")
	}
	defer rows.Close()
	var out []*merchant.Merchant
	for rows.Next() {
		m, err := scanMerchant(rows)
		if err != nil {
			return nil, mapError(err, "find expiring kyc")
		}
		out = append(out, m)
	}
	return out, mapError(rows.Err(), "find expiring kyc")
}

// --- scanning ----------------------------------------------------------------------------------

func scanMerchant(row scanRow) (*merchant.Merchant, error) {
	var (
		p           merchant.RehydrateParams
		id, tenant  string
		extRef      *string
		env, status string
		suspension  string
		kycStatus   string
		risk        string
		version     int64
		prof        merchant.BusinessProfile
		country     string
		mcc         string
		volAmt      int64
		volCcy      string
		tickAmt     int64
		tickCcy     string
		banksRaw    []byte
		princRaw    []byte
		attRaw      []byte
	)
	if err := row.Scan(
		&id, &tenant, &extRef, &p.LegalName, &p.DisplayName,
		&env, &status, &p.StatusReason, &suspension,
		&kycStatus, &p.KYCProviderRef, &p.KYCCompletedAt, &p.KYCExpiresAt, &risk,
		&p.CertificationID, &p.ActiveConfigVersion, &version,
		&p.CreatedAt, &p.UpdatedAt, &p.ActivatedAt, &p.SuspendedAt,
		&prof.LegalEntityType, &prof.RegistrationNumber, &prof.TaxID, &prof.TaxIDLast4,
		&prof.IncorporationDate, &country, &prof.AddressLine1, &prof.AddressLine2,
		&prof.City, &prof.Region, &prof.PostalCode,
		&prof.WebsiteURL, &prof.SupportEmail, &prof.SupportPhone, &mcc, &prof.Description,
		&volAmt, &volCcy, &tickAmt, &tickCcy,
		&banksRaw, &princRaw, &attRaw,
	); err != nil {
		return nil, err
	}

	prof.Country = shared.Country(country)
	prof.MCC = shared.MCC(mcc)
	prof.ExpectedMonthlyVolume = safeMoney(volAmt, volCcy)
	prof.ExpectedAverageTicket = safeMoney(tickAmt, tickCcy)

	p.ID = shared.MerchantID(id)
	p.TenantID = shared.TenantID(tenant)
	if extRef != nil {
		p.ExternalRef = *extRef
	}
	p.Environment = shared.Environment(env)
	p.Status = merchant.Status(status)
	p.SuspensionReason = merchant.SuspensionReason(suspension)
	p.KYCStatus = merchant.KYCStatus(kycStatus)
	p.RiskRating = merchant.RiskRating(risk)
	p.Version = shared.Version(version)
	p.Profile = prof

	var err error
	if p.BankAccounts, err = decodeBankAccounts(banksRaw); err != nil {
		return nil, err
	}
	if p.Principals, err = decodePrincipals(princRaw); err != nil {
		return nil, err
	}
	if p.Attestations, err = decodeAttestations(attRaw); err != nil {
		return nil, err
	}

	return merchant.Rehydrate(p)
}

func decodeBankAccounts(raw []byte) ([]merchant.BankAccount, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var rows []struct {
		ID            string `json:"id"`
		Country       string `json:"country"`
		Currency      string `json:"currency"`
		Holder        string `json:"holder"`
		AccountLast4  string `json:"account_last4"`
		RoutingLast4  string `json:"routing_last4"`
		IBANLast4     string `json:"iban_last4"`
		SecretRef     string `json:"secret_ref"`
		Status        string `json:"status"`
		ValidationRef string `json:"validation_ref"`
		ValidatedAt   *int64 `json:"validated_at"`
		IsDefault     bool   `json:"is_default"`
		FailureReason string `json:"failure_reason"`
	}
	if err := unmarshalJSON(raw, &rows, "merchant bank accounts"); err != nil {
		return nil, err
	}
	out := make([]merchant.BankAccount, 0, len(rows))
	for _, b := range rows {
		out = append(out, merchant.BankAccount{
			ID: b.ID, Country: shared.Country(b.Country), Currency: money.Currency(b.Currency),
			HolderName: b.Holder, AccountLast4: b.AccountLast4, RoutingLast4: b.RoutingLast4,
			IBANLast4: b.IBANLast4, SecretRef: b.SecretRef,
			Status: merchant.BankAccountStatus(b.Status), ValidationRef: b.ValidationRef,
			ValidatedAt: microsToTimePtr(b.ValidatedAt), IsDefault: b.IsDefault,
			FailureReason: b.FailureReason,
		})
	}
	return out, nil
}

func decodePrincipals(raw []byte) ([]merchant.Principal, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var rows []struct {
		ID              string `json:"id"`
		Role            string `json:"role"`
		FirstName       string `json:"first_name"`
		LastName        string `json:"last_name"`
		OwnershipPct    int    `json:"ownership_pct"`
		Country         string `json:"country"`
		VerificationRef string `json:"verification_ref"`
		Verified        bool   `json:"verified"`
	}
	if err := unmarshalJSON(raw, &rows, "merchant principals"); err != nil {
		return nil, err
	}
	out := make([]merchant.Principal, 0, len(rows))
	for _, p := range rows {
		out = append(out, merchant.Principal{
			ID: p.ID, Role: merchant.PrincipalRole(p.Role),
			FirstName: p.FirstName, LastName: p.LastName, OwnershipPct: p.OwnershipPct,
			Country: shared.Country(p.Country), VerificationRef: p.VerificationRef,
			Verified: p.Verified,
		})
	}
	return out, nil
}

func decodeAttestations(raw []byte) ([]merchant.ComplianceAttestation, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var rows []struct {
		Type       string `json:"type"`
		Reference  string `json:"reference"`
		AttestedBy string `json:"attested_by"`
		AttestedAt int64  `json:"attested_at"`
		ExpiresAt  int64  `json:"expires_at"`
		DocumentID string `json:"document_id"`
	}
	if err := unmarshalJSON(raw, &rows, "merchant attestations"); err != nil {
		return nil, err
	}
	out := make([]merchant.ComplianceAttestation, 0, len(rows))
	for _, a := range rows {
		out = append(out, merchant.ComplianceAttestation{
			Type: a.Type, Reference: a.Reference, AttestedBy: a.AttestedBy,
			AttestedAt: time.UnixMicro(a.AttestedAt).UTC(),
			ExpiresAt:  time.UnixMicro(a.ExpiresAt).UTC(),
			DocumentID: a.DocumentID,
		})
	}
	return out, nil
}
