-- 0014 down. Removes the provisioning functions.
--
-- It deliberately does NOT drop the partitions themselves: they hold the data, and dropping a
-- month of payments to undo a function definition is not a rollback, it is an incident. Detaching
-- and archiving partitions is the archival job's business, never a migration's.
DROP FUNCTION IF EXISTS pp.create_future_partitions(INTEGER);
DROP FUNCTION IF EXISTS pp.create_partition(TEXT, TIMESTAMPTZ);
DROP FUNCTION IF EXISTS pp.partition_name(TEXT, TIMESTAMPTZ);
