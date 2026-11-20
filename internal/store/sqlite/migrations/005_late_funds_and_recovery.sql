-- core.Transfer.NotBefore: the lower bound for a transfer's block time that
-- the core derives from the head it had processed when it first saw the
-- transfer. The default is the millisecond encoding of the zero time
-- ("unknown"): transfers recorded before this migration are never dated.
ALTER TABLE transfers ADD COLUMN not_before INTEGER NOT NULL DEFAULT -62135596800000;

-- Operator credentials: a generation that every password change bumps
-- (browsers remembered as "known devices" are bound to it), and the
-- one-time recovery code minted by `hmnpay reset-password`, stored as a
-- SHA-256 hash with its expiry and the number of failed uses.
ALTER TABLE operator ADD COLUMN generation INTEGER NOT NULL DEFAULT 0;
ALTER TABLE operator ADD COLUMN recovery_hash TEXT NOT NULL DEFAULT '';
ALTER TABLE operator ADD COLUMN recovery_expires_at INTEGER NOT NULL DEFAULT -62135596800000;
ALTER TABLE operator ADD COLUMN recovery_attempts INTEGER NOT NULL DEFAULT 0;
