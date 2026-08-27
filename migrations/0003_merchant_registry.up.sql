-- 0003 — BC-2: the merchant and its child entities.
--
-- merchants, merchant_business_profile, merchant_bank_accounts and merchant_principals are one
-- aggregate written in one transaction under one merchants.version check. They are four tables
-- rather than one because the child cardinalities are genuinely 1:N, and one wide table with
-- twenty nullable columns per bank account is not a simplification.

CREATE TABLE pp.merchants (
    merchant_id           TEXT        PRIMARY KEY
                          CHECK (merchant_id ~ '^mrc_[0-9A-HJKMNP-TV-Z]{26}$'),
    tenant_id             TEXT        NOT NULL,
    external_reference    TEXT,
    legal_name            TEXT        NOT NULL CHECK (legal_name <> ''),
    display_name          TEXT        NOT NULL CHECK (display_name <> ''),
    environment           TEXT        NOT NULL CHECK (environment IN ('sandbox', 'production')),
    status                TEXT        NOT NULL DEFAULT 'CREATED' CHECK (status IN (
                              'CREATED', 'VALIDATING', 'VALIDATION_FAILED',
                              'KYC_PENDING', 'KYC_APPROVED', 'KYC_FAILED',
                              'BANK_VALIDATED', 'BANK_VALIDATION_FAILED',
                              'GATEWAY_PROVISIONING', 'PROVISIONING_FAILED',
                              'CONFIGURING', 'CONFIGURATION_FAILED',
                              'SANDBOX_VALIDATION', 'CERTIFICATION', 'CERTIFICATION_FAILED',
                              'COMPLIANCE_REJECTED', 'APPROVED', 'PRODUCTION_READY',
                              'ACTIVE', 'SUSPENDED', 'TERMINATED')),
    status_reason         TEXT        NOT NULL DEFAULT '',
    suspension_reason     TEXT        NOT NULL DEFAULT '',
    kyc_status            TEXT        NOT NULL DEFAULT 'NOT_STARTED',
    kyc_provider_ref      TEXT        NOT NULL DEFAULT '',
    kyc_completed_at      TIMESTAMPTZ,
    kyc_expires_at        TIMESTAMPTZ,
    risk_rating           TEXT        NOT NULL DEFAULT 'STANDARD'
                          CHECK (risk_rating IN ('LOW', 'STANDARD', 'ELEVATED', 'HIGH')),
    certification_id      TEXT        NOT NULL DEFAULT '',
    active_config_version INTEGER     NOT NULL DEFAULT 0 CHECK (active_config_version >= 0),
    version               BIGINT      NOT NULL DEFAULT 0,
    created_at            TIMESTAMPTZ NOT NULL,
    updated_at            TIMESTAMPTZ NOT NULL,
    activated_at          TIMESTAMPTZ,
    suspended_at          TIMESTAMPTZ,
    terminated_at         TIMESTAMPTZ,

    -- baseline section 2: merchant_id is unique *within* a tenant. The composite key is what a
    -- foreign key from a child table targets, so a child can never point at a merchant in
    -- another tenant even if its own tenant_id were wrong.
    CONSTRAINT uq_merchant_tenant UNIQUE (tenant_id, merchant_id),
    CONSTRAINT merchants_active_has_time CHECK (status <> 'ACTIVE' OR activated_at IS NOT NULL)
);

COMMENT ON COLUMN pp.merchants.external_reference IS
    'The tenant''s own identifier for this merchant. Unique within a tenant when present, so a '
    'tenant can look a merchant up by their key without storing ours.';
COMMENT ON CONSTRAINT uq_merchant_tenant ON pp.merchants IS
    'Target of every child foreign key. Deliberately (tenant_id, merchant_id) and not '
    'merchant_id alone: a child row referencing only merchant_id could be attached across a '
    'tenant boundary by a bug that RLS would then hide rather than reject.';

CREATE UNIQUE INDEX uq_merchant_external_ref ON pp.merchants (tenant_id, external_reference)
    WHERE external_reference IS NOT NULL;
CREATE INDEX idx_merchants_status ON pp.merchants (tenant_id, status, merchant_id);
CREATE INDEX idx_merchants_list ON pp.merchants (tenant_id, created_at DESC, merchant_id DESC);
CREATE INDEX idx_merchants_kyc_expiry ON pp.merchants (kyc_expires_at)
    WHERE kyc_expires_at IS NOT NULL AND status <> 'TERMINATED';

COMMENT ON INDEX pp.idx_merchants_kyc_expiry IS
    'Serves FindKYCExpiring: re-verification has to start before processing stops, not after a '
    'merchant discovers their payments are being refused.';

ALTER TABLE pp.merchants ENABLE ROW LEVEL SECURITY;
ALTER TABLE pp.merchants FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON pp.merchants
    FOR ALL
    TO pp_app
    USING      (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE TABLE pp.merchant_business_profile (
    merchant_id                    TEXT        PRIMARY KEY,
    tenant_id                      TEXT        NOT NULL,
    legal_entity_type              TEXT        NOT NULL DEFAULT '',
    registration_number            TEXT        NOT NULL DEFAULT '',
    tax_id_ref                     TEXT        NOT NULL DEFAULT '',
    tax_id_last4                   TEXT        NOT NULL DEFAULT ''
                                   CHECK (tax_id_last4 = '' OR tax_id_last4 ~ '^[0-9A-Za-z]{1,4}$'),
    incorporation_date             DATE,
    country                        CHAR(2)     NOT NULL CHECK (country ~ '^[A-Z]{2}$'),
    address_line1                  TEXT        NOT NULL DEFAULT '',
    address_line2                  TEXT        NOT NULL DEFAULT '',
    city                           TEXT        NOT NULL DEFAULT '',
    region                         TEXT        NOT NULL DEFAULT '',
    postal_code                    TEXT        NOT NULL DEFAULT '',
    website_url                    TEXT        NOT NULL DEFAULT '',
    support_email                  TEXT        NOT NULL DEFAULT '',
    support_phone                  TEXT        NOT NULL DEFAULT '',
    mcc                            CHAR(4)     NOT NULL CHECK (mcc ~ '^[0-9]{4}$'),
    description                    TEXT        NOT NULL DEFAULT '',
    expected_monthly_volume        BIGINT      NOT NULL DEFAULT 0 CHECK (expected_monthly_volume >= 0),
    expected_monthly_volume_ccy    CHAR(3)     NOT NULL DEFAULT 'USD',
    expected_average_ticket        BIGINT      NOT NULL DEFAULT 0 CHECK (expected_average_ticket >= 0),
    expected_average_ticket_ccy    CHAR(3)     NOT NULL DEFAULT 'USD',

    FOREIGN KEY (tenant_id, merchant_id)
        REFERENCES pp.merchants (tenant_id, merchant_id) ON DELETE CASCADE
);

COMMENT ON COLUMN pp.merchant_business_profile.tax_id_ref IS
    'Secrets-manager reference. The full tax identifier is personal data with a residency and a '
    'crypto-shredding obligation; only the last four digits are stored in the clear, and those '
    'are what the UI and the logs may show.';

ALTER TABLE pp.merchant_business_profile ENABLE ROW LEVEL SECURITY;
ALTER TABLE pp.merchant_business_profile FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON pp.merchant_business_profile
    FOR ALL
    TO pp_app
    USING      (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE TABLE pp.merchant_bank_accounts (
    bank_account_id   TEXT        PRIMARY KEY,
    tenant_id         TEXT        NOT NULL,
    merchant_id       TEXT        NOT NULL,
    country           CHAR(2)     NOT NULL CHECK (country ~ '^[A-Z]{2}$'),
    currency          CHAR(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    holder_name       TEXT        NOT NULL DEFAULT '',
    account_last4     TEXT        NOT NULL DEFAULT '',
    routing_last4     TEXT        NOT NULL DEFAULT '',
    iban_last4        TEXT        NOT NULL DEFAULT '',
    secret_ref        TEXT        NOT NULL DEFAULT '',
    status            TEXT        NOT NULL DEFAULT 'UNVERIFIED' CHECK (status IN (
                          'UNVERIFIED', 'PENDING_VERIFICATION', 'VERIFIED', 'VERIFICATION_FAILED')),
    validation_ref    TEXT        NOT NULL DEFAULT '',
    validated_at      TIMESTAMPTZ,
    is_default        BOOLEAN     NOT NULL DEFAULT false,
    failure_reason    TEXT        NOT NULL DEFAULT '',
    sort_order        INTEGER     NOT NULL DEFAULT 0,

    FOREIGN KEY (tenant_id, merchant_id)
        REFERENCES pp.merchants (tenant_id, merchant_id) ON DELETE CASCADE,
    CONSTRAINT bank_accounts_no_pan CHECK (account_last4 !~ '^[0-9]{13,19}$'),
    CONSTRAINT bank_accounts_verified_has_time
        CHECK (status <> 'VERIFIED' OR validated_at IS NOT NULL)
);

-- "Exactly one primary account per settlement currency." A partial unique index rather than an
-- application check, because the failure mode of getting it wrong is money settled to the wrong
-- account and there is no compensating transaction for that.
CREATE UNIQUE INDEX uq_bank_primary
    ON pp.merchant_bank_accounts (tenant_id, merchant_id, currency)
    WHERE is_default;

CREATE INDEX idx_bank_accounts_merchant
    ON pp.merchant_bank_accounts (tenant_id, merchant_id, sort_order);

ALTER TABLE pp.merchant_bank_accounts ENABLE ROW LEVEL SECURITY;
ALTER TABLE pp.merchant_bank_accounts FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON pp.merchant_bank_accounts
    FOR ALL
    TO pp_app
    USING      (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE TABLE pp.merchant_principals (
    principal_id     TEXT        PRIMARY KEY,
    tenant_id        TEXT        NOT NULL,
    merchant_id      TEXT        NOT NULL,
    role             TEXT        NOT NULL CHECK (role IN (
                         'BENEFICIAL_OWNER', 'DIRECTOR', 'AUTHORISED_REPRESENTATIVE', 'EXECUTIVE')),
    first_name       TEXT        NOT NULL DEFAULT '',
    last_name        TEXT        NOT NULL DEFAULT '',
    ownership_pct    SMALLINT    NOT NULL DEFAULT 0 CHECK (ownership_pct BETWEEN 0 AND 100),
    country          CHAR(2)     NOT NULL DEFAULT 'XX',
    verification_ref TEXT        NOT NULL DEFAULT '',
    verified         BOOLEAN     NOT NULL DEFAULT false,
    is_ubo           BOOLEAN     NOT NULL GENERATED ALWAYS AS (ownership_pct >= 25) STORED,
    sort_order       INTEGER     NOT NULL DEFAULT 0,

    FOREIGN KEY (tenant_id, merchant_id)
        REFERENCES pp.merchants (tenant_id, merchant_id) ON DELETE CASCADE
);

COMMENT ON COLUMN pp.merchant_principals.is_ubo IS
    'Generated, not stored independently: a UBO is by definition a principal holding at least '
    '25 percent. A separate boolean would eventually disagree with the percentage, and the '
    'regulator asks for UBOs by the definition, not by our flag.';

CREATE INDEX idx_principals_ubo ON pp.merchant_principals (tenant_id, merchant_id) WHERE is_ubo;
CREATE INDEX idx_principals_merchant
    ON pp.merchant_principals (tenant_id, merchant_id, sort_order);

ALTER TABLE pp.merchant_principals ENABLE ROW LEVEL SECURITY;
ALTER TABLE pp.merchant_principals FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON pp.merchant_principals
    FOR ALL
    TO pp_app
    USING      (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE TABLE pp.merchant_attestations (
    tenant_id    TEXT        NOT NULL,
    merchant_id  TEXT        NOT NULL,
    type         TEXT        NOT NULL CHECK (type <> ''),
    reference    TEXT        NOT NULL DEFAULT '',
    attested_by  TEXT        NOT NULL DEFAULT '',
    attested_at  TIMESTAMPTZ NOT NULL,
    expires_at   TIMESTAMPTZ NOT NULL,
    document_id  TEXT        NOT NULL DEFAULT '',

    PRIMARY KEY (tenant_id, merchant_id, type),
    FOREIGN KEY (tenant_id, merchant_id)
        REFERENCES pp.merchants (tenant_id, merchant_id) ON DELETE CASCADE,
    CONSTRAINT attestation_expires_after_attested CHECK (expires_at > attested_at)
);

COMMENT ON CONSTRAINT attestation_expires_after_attested ON pp.merchant_attestations IS
    'An attestation with no expiry silently becomes stale, and a stale attestation is '
    'indistinguishable from a missing one at audit time. Requiring a future expiry makes the '
    'staleness a query rather than a discovery.';

ALTER TABLE pp.merchant_attestations ENABLE ROW LEVEL SECURITY;
ALTER TABLE pp.merchant_attestations FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON pp.merchant_attestations
    FOR ALL
    TO pp_app
    USING      (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
