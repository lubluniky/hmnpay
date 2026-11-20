-- core.PaymentLink.MinAmount: the least a customer may choose to pay on a
-- link without a fixed price, in base units. '0' means "the default" (one
-- whole unit of the asset), which is what every existing link gets.
ALTER TABLE links ADD COLUMN min_amount TEXT NOT NULL DEFAULT '0';
