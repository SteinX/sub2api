-- Scheduler cache publication compares versions of the accounts row. NOW()
-- alone uses the transaction start time and may move backwards when a waiting
-- older transaction commits after a newer transaction. Advance under the row
-- lock, at PostgreSQL's native microsecond precision, on every row update.
-- This does not version account_groups or other independently stored relations.
CREATE OR REPLACE FUNCTION advance_account_updated_at_monotonically()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    NEW.updated_at := GREATEST(clock_timestamp(), OLD.updated_at + INTERVAL '1 microsecond');
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS accounts_monotonic_updated_at ON accounts;
CREATE TRIGGER accounts_monotonic_updated_at
BEFORE UPDATE ON accounts
FOR EACH ROW EXECUTE FUNCTION advance_account_updated_at_monotonically();
