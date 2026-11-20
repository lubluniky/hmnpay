-- core.ChainHead.Final: the highest block at or below the processed head
-- that the chain adapter treats as beyond reorganisation; the start block of
-- the scan of an address handed out at that head. 0: the head carries none.
ALTER TABLE chain_head ADD COLUMN final INTEGER NOT NULL DEFAULT 0;
