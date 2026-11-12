-- Statistics for audit_logs.product_id, added just before with no rows
-- changed, so autovacuum would not analyze it soon: without them the
-- planner takes every line for one with a product, and the audit page
-- and activity feed, filtering by product, scan the table. ANALYZE
-- samples the table and takes no lock that holds up reads or writes.
ANALYZE audit_logs;
