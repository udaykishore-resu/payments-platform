-- 0012 down. IRREVERSIBLE: drops the outbox, discarding any event not yet published.
DROP TABLE IF EXISTS pp.outbox_events;
