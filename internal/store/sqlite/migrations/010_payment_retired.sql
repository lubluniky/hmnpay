-- core.Payment.RetiredAt / RetiredUnchecked: an address leaves the chain
-- watch list on evidence from the chain, and that it has left is recorded
-- (no longer recomputed from the clock on every read). The integer default
-- is the millisecond encoding of the zero time, as for every timestamp
-- column: on the list, or may come back to it.
ALTER TABLE payments ADD COLUMN retired_at INTEGER NOT NULL DEFAULT -62135596800000;
ALTER TABLE payments ADD COLUMN retired_unchecked INTEGER NOT NULL DEFAULT 0;
-- core.PaymentFilter.Contract: the watch list is the current asset's; the
-- payments of an asset configured before stay unretired until it is
-- configured again, and are not counted meanwhile.
ALTER TABLE payments ADD COLUMN asset_contract TEXT NOT NULL DEFAULT '';
UPDATE payments SET asset_contract = coalesce(json_extract(asset, '$.Contract'), '');
-- The watch list (open payments, and settled ones not retired) and the
-- payments due to leave it, per chain and asset.
CREATE INDEX payments_retirement ON payments (chain_id, asset_contract, retired_at, watch_until);
-- core.ChainHead.Checked: the block time before which every block has been
-- scanned by two log endpoints (zero: the head's own time).
ALTER TABLE chain_head ADD COLUMN checked INTEGER NOT NULL DEFAULT -62135596800000;
