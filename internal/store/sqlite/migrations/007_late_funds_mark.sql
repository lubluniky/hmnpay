-- core.Payment.LateFundsMark: the part of amount_confirmed that a settled
-- payment's outcome, a raised late-funds issue or an operator's resolution
-- has accounted for; confirmed funds beyond it are flagged as late funds
-- once they add up to the dust limit. Everything confirmed on an existing
-- payment counts as accounted for: nothing that settled before this
-- migration is flagged again because of it. (The value is not used while a
-- payment is open; it is set when the payment settles.)
ALTER TABLE payments ADD COLUMN late_funds_mark TEXT NOT NULL DEFAULT '0';
UPDATE payments SET late_funds_mark = amount_confirmed;
