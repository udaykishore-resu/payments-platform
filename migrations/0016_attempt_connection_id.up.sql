-- 0016_attempt_connection_id — EXPAND: give a payment attempt a reference to the merchant-to-
-- gateway connection it was dispatched over.
--
-- WHY THIS EXISTS
--   api/openapi/payments-platform.v1.yaml requires `connectionId` on every PaymentAttempt, and
--   until now the platform could not populate it: internal/domain/payment.Attempt held a gateway
--   but no reference to the gateway.Connection aggregate, so the value was not reachable from the
--   aggregate the handler renders. It was recorded as a known contract gap rather than filled with
--   a fabricated identifier, because an identifier in a response that resolves to nothing is worse
--   than an absent one.
--
--   The gateway alone is not enough. One merchant can hold several connections to the same
--   gateway — a live one and one being re-provisioned, or two sub-accounts in different corridors
--   — and the credential, the external account and the certification state all belong to the
--   *connection*. Without this column, an attempt that failed authentication cannot be traced to
--   the credential it used, which is the first question asked when a credential rotation goes
--   wrong.
--
-- WHAT 0007 ALREADY DID, AND WHY THIS IS STILL A NEW MIGRATION
--   0007_payments declared `gateway_connection_id TEXT NOT NULL DEFAULT ''` and then nothing ever
--   wrote to it: no repository column, no domain field, no renderer. The column existed and the
--   data did not.
--
--   0007 is applied everywhere and its checksum is recorded, so editing it is not an option
--   (migrations/README.md §2.1: the runner refuses a changed checksum, and fixing forward is the
--   rule). This migration therefore uses ADD COLUMN IF NOT EXISTS: a no-op on every database
--   created by 0007, and correct on one restored from a dump that predates it. What it actually
--   contributes is the part that was missing — the well-formedness constraint and the column
--   documentation — plus the backfill, which is deliberately NOT run here; see below.
--
-- EXPAND / CONTRACT (migrations/README.md §1)
--   Expand only. Every statement below is one the *old* binary tolerates: it does not read the
--   column, and a nullable column with a NOT VALID check changes nothing it can observe. There is
--   no contract phase planned — the column stays nullable, because an attempt written before this
--   change legitimately has no connection to name and NOT NULL would make those rows a lie.

ALTER TABLE pp.payment_attempts
    ADD COLUMN IF NOT EXISTS gateway_connection_id TEXT;

-- NOT VALID, per §4's recipe: a plain ADD CHECK takes ACCESS EXCLUSIVE and scans every partition,
-- which on the payment path is an outage. NOT VALID applies the rule to new and updated rows
-- immediately and leaves the historical scan to a later VALIDATE CONSTRAINT, which takes only
-- SHARE UPDATE EXCLUSIVE.
--
-- The empty string is accepted alongside NULL because 0007's column is NOT NULL DEFAULT '' and
-- every existing row therefore holds ''. Accepting both is what lets this constraint be added
-- without first rewriting history; the domain treats '' and NULL identically as "not recorded".
ALTER TABLE pp.payment_attempts
    ADD CONSTRAINT attempt_connection_ref_well_formed
    CHECK (gateway_connection_id IS NULL
           OR gateway_connection_id = ''
           OR gateway_connection_id ~ '^gwc_[0-9A-HJKMNP-TV-Z]{26}$') NOT VALID;

COMMENT ON COLUMN pp.payment_attempts.gateway_connection_id IS
    'The pp.gateway_connections row this attempt was dispatched over, stamped before the attempt '
    'is committed and therefore before the gateway is called. Distinct from gateway_id: the '
    'credential, the external account reference and the certification state belong to the '
    'connection, so this is what makes "which credential signed this request" answerable from the '
    'attempt row rather than by re-deriving it from the merchant''s connections as they stand '
    'today — which is the wrong answer the moment a connection is revoked or re-provisioned. '
    'NULL or empty for attempts recorded before migration 0016; deliberately not NOT NULL, '
    'because those attempts genuinely have no connection to name.';

-- No index. The column is read as part of an attempt row that is always fetched by payment id
-- through the existing primary key and partition pruning; there is no query that filters on it.
-- Adding an index "in case" to a range-partitioned table on the money path costs write amplitude
-- on every dispatch forever, and CREATE INDEX CONCURRENTLY is not available on a partitioned
-- parent, so the index would have to be built per partition by hand. If a
-- "show me every attempt on this connection" view is ever needed, it belongs in a later migration
-- written against the query it serves.
--
-- THE BACKFILL IS NOT HERE, AND THAT IS THE POINT
--   migrations/README.md §1 forbids running a backfill inside a migration, and payment_attempts is
--   the table where the reason bites hardest: an unbatched UPDATE across eighty-four partitions
--   holds one transaction over the money path for as long as it takes, and the lock_timeout that
--   protects everything else does not save a statement that has already acquired its locks.
--
--   Run it out of band, after this migration has applied, in batches:
--
--     UPDATE pp.payment_attempts a
--        SET gateway_connection_id = c.connection_id
--       FROM pp.payments p
--       JOIN pp.gateway_connections c
--         ON c.merchant_id = p.merchant_id
--        AND c.tenant_id   = p.tenant_id
--      WHERE a.payment_id      = p.payment_id
--        AND a.partition_month = p.partition_month
--        AND c.gateway_id      = a.gateway_id
--        AND coalesce(a.gateway_connection_id, '') = ''
--        AND a.partition_month = $1        -- one partition at a time
--        AND a.attempt_id IN (SELECT attempt_id FROM pp.payment_attempts
--                              WHERE partition_month = $1
--                                AND coalesce(gateway_connection_id, '') = ''
--                              LIMIT 10000);
--
--   The join is exact where a merchant has one connection per gateway, which is the overwhelmingly
--   common shape. Where a merchant holds several connections to one gateway the historical
--   attribution is genuinely ambiguous — the information needed to disambiguate was never
--   recorded — so those rows are left blank rather than guessed at. A blank is honest; a
--   plausible-looking wrong connection id would be read as evidence in a rotation post-mortem.
--
--   Once the backfill has run and a release has passed:
--     ALTER TABLE pp.payment_attempts VALIDATE CONSTRAINT attempt_connection_ref_well_formed;
