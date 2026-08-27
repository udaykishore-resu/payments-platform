-- 0011 down. IRREVERSIBLE: drops the audit chain and every partition of it. Never run against an
-- environment under a 7-year WORM retention obligation; the archived copies in S3 Object Lock are
-- the evidence of record and are not affected by this script.
DROP TABLE IF EXISTS pp.audit_records;
