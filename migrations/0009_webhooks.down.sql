-- 0009 down. IRREVERSIBLE: drops BC-7 tables and the consumer dedup table.
DROP TABLE IF EXISTS pp.event_dedup;
DROP TABLE IF EXISTS pp.webhook_dedup;
DROP TABLE IF EXISTS pp.inbound_webhooks;
