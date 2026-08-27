-- 0015 — reference data and the gateway registry seed.
--
-- These tables exist so that the database can validate what the application validates. A
-- currency code with no exponent is not a currency; a foreign key to this table turns
-- "unsupported currency" from a runtime surprise at the gateway into a rejected write.

CREATE TABLE pp.currencies (
    code         CHAR(3)  PRIMARY KEY CHECK (code ~ '^[A-Z]{3}$'),
    exponent     SMALLINT NOT NULL CHECK (exponent BETWEEN 0 AND 4),
    numeric_code CHAR(3)  NOT NULL CHECK (numeric_code ~ '^[0-9]{3}$'),
    name         TEXT     NOT NULL
);

COMMENT ON TABLE pp.currencies IS
    'The supported ISO 4217 set with its minor-unit exponent. A curated list rather than the '
    'full ISO table: an unlisted currency is a configuration error we want surfaced at '
    'validation time, not a value silently accepted with a guessed exponent. Note the exponents '
    'that catch people out - JPY and KRW have none, the Gulf dinars have three, CLF has four. '
    'Code that assumes "cents" is wrong for roughly a quarter of the world''s payment volume.';

INSERT INTO pp.currencies (code, exponent, numeric_code, name) VALUES
    ('AED', 2, '784', 'UAE Dirham'),
    ('AUD', 2, '036', 'Australian Dollar'),
    ('BHD', 3, '048', 'Bahraini Dinar'),
    ('BRL', 2, '986', 'Brazilian Real'),
    ('CAD', 2, '124', 'Canadian Dollar'),
    ('CHF', 2, '756', 'Swiss Franc'),
    ('CLF', 4, '990', 'Unidad de Fomento'),
    ('CLP', 0, '152', 'Chilean Peso'),
    ('CNY', 2, '156', 'Yuan Renminbi'),
    ('COP', 2, '170', 'Colombian Peso'),
    ('CZK', 2, '203', 'Czech Koruna'),
    ('DKK', 2, '208', 'Danish Krone'),
    ('EUR', 2, '978', 'Euro'),
    ('GBP', 2, '826', 'Pound Sterling'),
    ('HKD', 2, '344', 'Hong Kong Dollar'),
    ('HUF', 2, '348', 'Forint'),
    ('IDR', 2, '360', 'Rupiah'),
    ('ILS', 2, '376', 'New Israeli Sheqel'),
    ('INR', 2, '356', 'Indian Rupee'),
    ('ISK', 0, '352', 'Iceland Krona'),
    ('JOD', 3, '400', 'Jordanian Dinar'),
    ('JPY', 0, '392', 'Yen'),
    ('KRW', 0, '410', 'Won'),
    ('KWD', 3, '414', 'Kuwaiti Dinar'),
    ('MXN', 2, '484', 'Mexican Peso'),
    ('MYR', 2, '458', 'Malaysian Ringgit'),
    ('NOK', 2, '578', 'Norwegian Krone'),
    ('NZD', 2, '554', 'New Zealand Dollar'),
    ('OMR', 3, '512', 'Rial Omani'),
    ('PHP', 2, '608', 'Philippine Peso'),
    ('PLN', 2, '985', 'Zloty'),
    ('RON', 2, '946', 'Romanian Leu'),
    ('SAR', 2, '682', 'Saudi Riyal'),
    ('SEK', 2, '752', 'Swedish Krona'),
    ('SGD', 2, '702', 'Singapore Dollar'),
    ('THB', 2, '764', 'Baht'),
    ('TND', 3, '788', 'Tunisian Dinar'),
    ('TRY', 2, '949', 'Turkish Lira'),
    ('TWD', 2, '901', 'New Taiwan Dollar'),
    ('USD', 2, '840', 'US Dollar'),
    ('VND', 0, '704', 'Dong'),
    ('ZAR', 2, '710', 'Rand')
ON CONFLICT (code) DO NOTHING;

CREATE TABLE pp.payment_methods (
    code                     TEXT    PRIMARY KEY CHECK (code ~ '^[A-Z][A-Z_]*$'),
    supports_separate_capture BOOLEAN NOT NULL,
    is_asynchronous          BOOLEAN NOT NULL,
    sca_in_scope             BOOLEAN NOT NULL,
    refundable               BOOLEAN NOT NULL
);

COMMENT ON TABLE pp.payment_methods IS
    'Deliberately coarse categories, not a gateway''s taxonomy. Stripe alone has dozens of '
    'payment_method_types; mapping that granularity into the core would couple the platform to '
    'whichever gateway was integrated first. The adapters translate; these are the categories '
    'that change platform behaviour.';

INSERT INTO pp.payment_methods
    (code, supports_separate_capture, is_asynchronous, sca_in_scope, refundable) VALUES
    ('CARD',       true,  false, true,  true),
    ('APPLE_PAY',  true,  false, true,  true),
    ('GOOGLE_PAY', true,  false, true,  true),
    ('PAYPAL',     true,  false, false, true),
    ('SEPA_DEBIT', false, true,  false, true),
    ('ACH_DEBIT',  false, true,  false, true),
    ('IDEAL',      false, false, false, true),
    ('SOFORT',     false, true,  false, true),
    ('BANCONTACT', false, false, false, true),
    ('UPI',        false, false, false, true),
    ('BLIK',       false, false, false, true)
ON CONFLICT (code) DO NOTHING;

REVOKE INSERT, UPDATE, DELETE ON pp.currencies, pp.payment_methods FROM pp_app;
GRANT SELECT ON pp.currencies, pp.payment_methods TO pp_app, pp_readonly;

-- System roles. is_system rows are immutable; the scope vocabulary is baseline section 19.
INSERT INTO pp.roles (role_id, name, scopes, is_system) VALUES
    ('role_platform_admin', 'platform-admin',
     ARRAY['tenants:write','merchants:write','gateways:write','config:write','audit:read'], true),
    ('role_tenant_admin', 'tenant-admin',
     ARRAY['merchants:write','config:write','payments:read','audit:read'], true),
    ('role_merchant_operator', 'merchant-operator',
     ARRAY['payments:read','payments:capture','payments:refund','payments:void'], true),
    ('role_readonly', 'readonly', ARRAY['payments:read','merchants:read','config:read'], true)
ON CONFLICT (role_id) DO NOTHING;

-- The gateway registry.
--
-- Capability descriptors are declarative and fail closed: an empty country list means "no
-- country", never "all countries". The failure mode of the other choice is routing a Brazilian
-- merchant to a gateway with no Brazilian licence and finding out at settlement.
INSERT INTO pp.gateways
    (gateway_id, display_name, vendor, api_version, base_urls, capabilities, cost_model,
     signature_scheme, status, regions)
VALUES
(
    'stripe', 'Stripe', 'Stripe, Inc.', '2024-06-20',
    '{"sandbox":"https://api.stripe.com","production":"https://api.stripe.com"}',
    '{
      "countries": ["US","CA","GB","IE","FR","DE","ES","IT","NL","BE","AT","SE","NO","DK","FI",
                    "PL","PT","CH","AU","NZ","SG","JP","HK","MX","BR"],
      "currencies": ["USD","EUR","GBP","CAD","AUD","NZD","SGD","JPY","HKD","CHF","SEK","NOK",
                     "DKK","PLN","MXN","BRL"],
      "methods": ["CARD","APPLE_PAY","GOOGLE_PAY","SEPA_DEBIT","ACH_DEBIT","IDEAL","SOFORT",
                  "BANCONTACT","BLIK"],
      "operations": ["authorize","capture","refund","void","lookup","provision","webhook_register"],
      "supportsPartialCapture": true,
      "supportsMultipleCaptures": false,
      "supportsPartialRefund": true,
      "supportsVoid": true,
      "supports3DS2": true,
      "supportsNetworkTokens": true,
      "supportsIdempotencyKeys": true,
      "maxRefundWindowDays": 180,
      "authorizationValidityHours": 168
    }',
    '{"rates":[{"currency":"USD","method":"*","basisPoints":290,"fixedFee":30},
               {"currency":"EUR","method":"*","basisPoints":150,"fixedFee":25},
               {"currency":"GBP","method":"*","basisPoints":150,"fixedFee":20}]}',
    'HMAC_SHA256', 'ACTIVE', ARRAY['us-east-1','eu-west-1','ap-southeast-1']
),
(
    'adyen', 'Adyen', 'Adyen N.V.', 'v71',
    '{"sandbox":"https://checkout-test.adyen.com","production":"https://checkout-live.adyen.com"}',
    '{
      "countries": ["GB","IE","FR","DE","ES","IT","NL","BE","AT","SE","NO","DK","FI","PL","PT",
                    "CH","US","CA","AU","NZ","SG","JP","HK","IN","BR","MX"],
      "currencies": ["EUR","GBP","USD","CAD","AUD","NZD","SGD","JPY","HKD","CHF","SEK","NOK",
                     "DKK","PLN","INR","BRL","MXN"],
      "methods": ["CARD","APPLE_PAY","GOOGLE_PAY","SEPA_DEBIT","IDEAL","SOFORT","BANCONTACT",
                  "BLIK","UPI"],
      "operations": ["authorize","capture","refund","void","lookup","provision","webhook_register"],
      "supportsPartialCapture": true,
      "supportsMultipleCaptures": true,
      "supportsPartialRefund": true,
      "supportsVoid": true,
      "supports3DS2": true,
      "supportsNetworkTokens": true,
      "supportsIdempotencyKeys": true,
      "maxRefundWindowDays": 365,
      "authorizationValidityHours": 672
    }',
    '{"rates":[{"currency":"EUR","method":"*","basisPoints":60,"fixedFee":11},
               {"currency":"GBP","method":"*","basisPoints":60,"fixedFee":10},
               {"currency":"USD","method":"*","basisPoints":80,"fixedFee":12}]}',
    'HMAC_SHA256', 'ACTIVE', ARRAY['eu-west-1','us-east-1','ap-southeast-1']
),
(
    'paypal', 'PayPal', 'PayPal Holdings, Inc.', 'v2',
    '{"sandbox":"https://api-m.sandbox.paypal.com","production":"https://api-m.paypal.com"}',
    '{
      "countries": ["US","CA","GB","IE","FR","DE","ES","IT","NL","BE","AT","SE","NO","DK","FI",
                    "PL","PT","CH","AU","NZ","SG","JP","HK","MX","BR"],
      "currencies": ["USD","EUR","GBP","CAD","AUD","NZD","SGD","JPY","HKD","CHF","SEK","NOK",
                     "DKK","PLN","MXN","BRL"],
      "methods": ["PAYPAL","CARD"],
      "operations": ["authorize","capture","refund","void","lookup","webhook_register"],
      "supportsPartialCapture": true,
      "supportsMultipleCaptures": true,
      "supportsPartialRefund": true,
      "supportsVoid": true,
      "supports3DS2": false,
      "supportsNetworkTokens": false,
      "supportsIdempotencyKeys": true,
      "maxRefundWindowDays": 180,
      "authorizationValidityHours": 720
    }',
    '{"rates":[{"currency":"USD","method":"PAYPAL","basisPoints":349,"fixedFee":49},
               {"currency":"EUR","method":"PAYPAL","basisPoints":349,"fixedFee":39},
               {"currency":"GBP","method":"PAYPAL","basisPoints":349,"fixedFee":30}]}',
    'JWS', 'ACTIVE', ARRAY['us-east-1','eu-west-1']
)
ON CONFLICT (gateway_id) DO NOTHING;

-- Health rows for every (gateway, money-moving operation), created HEALTHY. They exist from the
-- start so the first orchestrator to boot reads a row rather than inventing a default, and so
-- `SELECT ... FOR UPDATE` on a health row never has to handle a missing row on the hot path.
INSERT INTO pp.gateway_health (gateway_id, operation, state, state_changed_at)
SELECT g.gateway_id, op, 'HEALTHY', now()
FROM pp.gateways g
CROSS JOIN unnest(ARRAY['authorize','capture','refund','void','lookup']) AS op
ON CONFLICT (gateway_id, operation) DO NOTHING;
