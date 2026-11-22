-- The author deleted the statement (allowed only while it has no arguments).
-- The contract is gone; the row stays so voters can still find and withdraw
-- their votes.
alter table statements add column deleted_at timestamp;
