-- 0007 down. IRREVERSIBLE: drops the money tables and every partition attached to them.
DROP TABLE IF EXISTS pp.payment_event_log;
DROP TABLE IF EXISTS pp.routing_plans;
DROP TABLE IF EXISTS pp.refunds;
DROP TABLE IF EXISTS pp.payment_attempts;
DROP TABLE IF EXISTS pp.payments;
