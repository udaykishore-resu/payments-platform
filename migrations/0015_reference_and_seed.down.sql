-- 0015 down. Removes the seeded reference data and registry entries.
--
-- Deleting a gateway row while connections reference it is refused by the foreign key, which is
-- the correct outcome: de-provisioning connections is an operational procedure, not something a
-- schema rollback gets to do implicitly.
DELETE FROM pp.gateway_health
 WHERE gateway_id IN ('stripe', 'adyen', 'paypal');
DELETE FROM pp.gateways
 WHERE gateway_id IN ('stripe', 'adyen', 'paypal');
DELETE FROM pp.roles WHERE is_system;
-- IRREVERSIBLE: the reference tables are recreated with their contents by the up migration.
DROP TABLE IF EXISTS pp.payment_methods;
DROP TABLE IF EXISTS pp.currencies;
