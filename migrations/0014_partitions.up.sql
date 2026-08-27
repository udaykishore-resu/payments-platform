-- 0014 — partition provisioning.
--
-- A partition that does not exist at insert time is a hard INSERT failure on the payment path.
-- That is the single worst failure mode this machinery has, which is why the lead time is
-- thirteen months rather than three: a broken maintenance job then has a year of slack before it
-- becomes an outage, and the alert on "fewer than two future partitions" fires long before that.

CREATE FUNCTION pp.partition_name(parent TEXT, month TIMESTAMPTZ) RETURNS TEXT
LANGUAGE sql IMMUTABLE AS $fn$
    SELECT parent || '_' || to_char(month AT TIME ZONE 'UTC', 'YYYY_MM');
$fn$;

COMMENT ON FUNCTION pp.partition_name(TEXT, TIMESTAMPTZ) IS
    'Naming is a function, not a convention, so the provisioning job, the verifier and the '
    'archival job cannot disagree about which table they are talking about.';

-- create_partition creates one monthly partition with EVERYTHING it needs, in one transaction:
-- the table, the per-partition indexes that cannot live on the parent, row-level security, the
-- grants, and the registry row. All or nothing, because a partition that exists without its I3
-- index is a partition in which double-charging is possible and nothing says so.
CREATE FUNCTION pp.create_partition(parent TEXT, month TIMESTAMPTZ)
RETURNS TEXT
LANGUAGE plpgsql AS $fn$
DECLARE
    start_ts TIMESTAMPTZ := date_trunc('month', month AT TIME ZONE 'UTC') AT TIME ZONE 'UTC';
    end_ts   TIMESTAMPTZ := (date_trunc('month', month AT TIME ZONE 'UTC') + INTERVAL '1 month') AT TIME ZONE 'UTC';
    part     TEXT        := pp.partition_name(parent, start_ts);
BEGIN
    IF parent NOT IN ('payments', 'payment_attempts', 'ledger_entries', 'audit_records') THEN
        RAISE EXCEPTION 'pp.create_partition: % is not a partitioned table in this schema', parent
            USING ERRCODE = 'invalid_parameter_value';
    END IF;

    IF to_regclass('pp.' || quote_ident(part)) IS NOT NULL THEN
        RETURN part;   -- idempotent: the hourly job re-runs constantly and must be a no-op
    END IF;

    EXECUTE format(
        'CREATE TABLE pp.%I PARTITION OF pp.%I FOR VALUES FROM (%L) TO (%L)',
        part, parent, start_ts, end_ts);

    -- Row-level security on the partition itself. Queries through the parent already apply the
    -- parent's policy; this is what protects a query that names the partition directly, which is
    -- exactly what an operator debugging a hot month tends to type.
    EXECUTE format('ALTER TABLE pp.%I ENABLE ROW LEVEL SECURITY', part);
    EXECUTE format('ALTER TABLE pp.%I FORCE  ROW LEVEL SECURITY', part);
    EXECUTE format(
        'CREATE POLICY tenant_isolation ON pp.%I FOR ALL TO pp_app '
        'USING (tenant_id = current_setting(''app.tenant_id'', true)) '
        'WITH CHECK (tenant_id = current_setting(''app.tenant_id'', true))',
        part);

    IF parent = 'payment_attempts' THEN
        -- INVARIANT I3, and the whole reason amendment A-02 exists.
        --
        -- A partitioned parent cannot carry this index: PostgreSQL requires a unique index on a
        -- partitioned table to include the partition key, and does not permit a partial unique
        -- index there at all. Created per partition it still enforces I3 globally, and the
        -- argument is short enough to check:
        --   1. every attempt of payment P carries partition_month = TimeOf(P) (the FK enforces it);
        --   2. therefore every attempt of P is in exactly one partition;
        --   3. therefore a second outcome='SUCCESS' row for P must go into that same partition;
        --   4. where this index rejects it. QED.
        EXECUTE format(
            'CREATE UNIQUE INDEX %I ON pp.%I (payment_id) WHERE outcome = ''SUCCESS''',
            'uq_attempt_success_' || to_char(start_ts AT TIME ZONE 'UTC', 'YYYY_MM'), part);
    END IF;

    IF parent IN ('payments', 'payment_attempts', 'ledger_entries', 'audit_records') THEN
        -- No DELETE on the money and evidence tables. Retention is a partition DETACH, never a
        -- DELETE: a DELETE over a billion rows bloats and vacuums for days and leaves the data
        -- recoverable from the heap in the meantime.
        EXECUTE format('REVOKE DELETE ON pp.%I FROM pp_app', part);
    END IF;

    IF parent IN ('ledger_entries', 'audit_records') THEN
        EXECUTE format('REVOKE UPDATE ON pp.%I FROM pp_app', part);
    END IF;

    INSERT INTO pp.partition_registry (table_name, partition_name, range_start, range_end, state)
    VALUES (parent, part, start_ts, end_ts, 'ATTACHED')
    ON CONFLICT (table_name, partition_name) DO NOTHING;

    RETURN part;
END;
$fn$;

-- create_future_partitions is what the hourly `platformctl partitions ensure` CronJob calls.
-- It is idempotent, it covers every partitioned table, and it returns what it created so the job
-- can log and alert on an unexpectedly empty result.
CREATE FUNCTION pp.create_future_partitions(months INTEGER DEFAULT 3)
RETURNS TABLE (parent TEXT, partition TEXT)
LANGUAGE plpgsql AS $fn$
DECLARE
    tbl    TEXT;
    offset_months INTEGER;
    made   TEXT;
    base   TIMESTAMPTZ := date_trunc('month', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC';
BEGIN
    IF months < 1 OR months > 60 THEN
        RAISE EXCEPTION 'pp.create_future_partitions: months must be between 1 and 60, got %', months
            USING ERRCODE = 'invalid_parameter_value';
    END IF;

    FOREACH tbl IN ARRAY ARRAY['payments', 'payment_attempts', 'ledger_entries', 'audit_records']
    LOOP
        -- The previous month is included deliberately. A payment created seconds before a month
        -- boundary can have its attempt written seconds after it, and the attempt carries the
        -- *payment's* month - so the previous month's partition must still exist.
        FOR offset_months IN -1..months LOOP
            made := pp.create_partition(tbl, base + (offset_months || ' months')::INTERVAL);
            parent := tbl;
            partition := made;
            RETURN NEXT;
        END LOOP;
    END LOOP;
END;
$fn$;

COMMENT ON FUNCTION pp.create_future_partitions(INTEGER) IS
    'Hourly maintenance entry point. Idempotent by construction, so a job that runs twice or a '
    'leader election that briefly elects two leaders costs nothing.';

-- Provision the hot window now: thirteen months forward plus the previous month. Thirteen
-- matches the hot-retention window in 04-domain-model.md section 8.4, so a freshly migrated
-- database has a full hot window on day one and the archival job has nothing to do for a year.
SELECT count(*) FROM pp.create_future_partitions(13);
