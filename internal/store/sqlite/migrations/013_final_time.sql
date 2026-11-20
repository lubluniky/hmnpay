-- core.ChainHead.FinalTime: the timestamp of the final block; an address
-- leaves the watch list only once the final block, too, is past its window.
-- The integer default is the millisecond encoding of the zero time: not known.
ALTER TABLE chain_head ADD COLUMN final_time INTEGER NOT NULL DEFAULT -62135596800000;
-- A head stored before this version may carry a final block that the chain
-- adapter took from depth alone, on a network that has finality (when the
-- endpoints did not report it, or after the adapter lost its checkpoint):
-- such a head is dropped, as in 012, and the core waits for the adapter's
-- first head, whose final block two endpoints attest (chain_not_ready).
DELETE FROM chain_head;
-- core.Payment.OldestPendingBlock: the lowest block among a payment's
-- pending transfers ('0': none, or recorded before; the payment's newest
-- pending block stands in for it until the payment is evaluated again).
ALTER TABLE payments ADD COLUMN oldest_pending_block TEXT NOT NULL DEFAULT '0';
