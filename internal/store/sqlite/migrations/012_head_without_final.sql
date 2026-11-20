-- core.ChainHead.Final is what a transfer must be at or below to be final,
-- and the start block of a new payment's scan. A head stored before the
-- chain adapter reported it has none: migration 011 gave it 0, which is a
-- block like any other, not "unknown". Such a head is dropped; the core has
-- no head until the adapter's first one (payments are refused meanwhile:
-- chain_not_ready), and the adapter resumes from its own checkpoint.
DELETE FROM chain_head;
