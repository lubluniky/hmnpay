CREATE TABLE payments (
    id TEXT NOT NULL PRIMARY KEY,
    status TEXT NOT NULL,
    amount TEXT NOT NULL,
    asset TEXT NOT NULL,
    address TEXT NOT NULL UNIQUE,
    address_index INTEGER NOT NULL,
    order_id TEXT NOT NULL,
    description TEXT NOT NULL,
    customer_email TEXT NOT NULL,
    redirect_url TEXT NOT NULL,
    metadata TEXT NOT NULL,
    link_id TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    required_confirmations INTEGER NOT NULL,
    tolerance TEXT NOT NULL,
    amount_received TEXT NOT NULL,
    amount_confirmed TEXT NOT NULL,
    tx_hash TEXT NOT NULL,
    from_address TEXT NOT NULL,
    pending_block TEXT NOT NULL,
    created_at_block TEXT NOT NULL,
    attention TEXT NOT NULL,
    resolution TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    paid_at INTEGER NOT NULL,
    settled_at INTEGER NOT NULL,
    watch_until INTEGER NOT NULL,
    amount_key TEXT NOT NULL,
    chain_id TEXT NOT NULL,
    needs_attention INTEGER NOT NULL
);

CREATE INDEX payments_created_at ON payments (created_at, id);
CREATE INDEX payments_amount_key ON payments (amount_key, id);
CREATE INDEX payments_status ON payments (status);
CREATE INDEX payments_needs_attention ON payments (needs_attention);
CREATE INDEX payments_order_id ON payments (order_id);
CREATE INDEX payments_link_id ON payments (link_id);
CREATE INDEX payments_chain_id ON payments (chain_id);
CREATE INDEX payments_paid_at ON payments (paid_at);
CREATE INDEX payments_expires_at ON payments (expires_at);
CREATE INDEX payments_watch_until ON payments (watch_until);
CREATE UNIQUE INDEX payments_idempotency_key ON payments (idempotency_key) WHERE idempotency_key <> '';

CREATE TABLE transfers (
    tx_hash TEXT NOT NULL,
    log_index TEXT NOT NULL,
    block_hash TEXT NOT NULL,
    payment_id TEXT NOT NULL,
    block_number TEXT NOT NULL,
    block_time INTEGER NOT NULL,
    sender TEXT NOT NULL,
    recipient TEXT NOT NULL,
    amount TEXT NOT NULL,
    status TEXT NOT NULL,
    seen_at INTEGER NOT NULL,
    confirmed_at INTEGER NOT NULL,
    orphaned_at INTEGER NOT NULL,
    PRIMARY KEY (tx_hash, log_index, block_hash)
);

CREATE INDEX transfers_payment_id ON transfers (payment_id, block_number, log_index, block_hash, tx_hash);
CREATE INDEX transfers_tx_hash ON transfers (tx_hash, block_number, log_index, block_hash);
CREATE INDEX transfers_status ON transfers (status, block_number, log_index, block_hash, tx_hash);

CREATE TABLE links (
    id TEXT NOT NULL PRIMARY KEY,
    slug TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL,
    description TEXT NOT NULL,
    amount TEXT NOT NULL,
    asset TEXT NOT NULL,
    active INTEGER NOT NULL,
    max_uses INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    redirect_url TEXT NOT NULL,
    uses INTEGER NOT NULL,
    total_collected TEXT NOT NULL,
    last_paid_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE INDEX links_created_at ON links (created_at, id);

CREATE TABLE api_keys (
    id TEXT NOT NULL PRIMARY KEY,
    name TEXT NOT NULL,
    mode TEXT NOT NULL,
    prefix TEXT NOT NULL,
    last4 TEXT NOT NULL,
    secret_hash TEXT NOT NULL UNIQUE,
    scopes TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    last_used_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    revoked_at INTEGER NOT NULL
);

CREATE INDEX api_keys_created_at ON api_keys (created_at, id);

CREATE TABLE sessions (
    id TEXT NOT NULL PRIMARY KEY,
    operator_id TEXT NOT NULL,
    token_hash TEXT NOT NULL UNIQUE,
    user_agent TEXT NOT NULL,
    ip TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);

CREATE INDEX sessions_expires_at ON sessions (expires_at);

CREATE TABLE operator (
    id TEXT NOT NULL,
    email TEXT NOT NULL,
    password_hash TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    singleton INTEGER PRIMARY KEY CHECK(singleton = 1)
);

CREATE TABLE settings (
    setup_completed INTEGER NOT NULL,
    public_url TEXT NOT NULL,
    merchant TEXT NOT NULL,
    payments TEXT NOT NULL,
    network TEXT NOT NULL,
    asset TEXT NOT NULL,
    wallet TEXT NOT NULL,
    updated_at INTEGER NOT NULL,
    singleton INTEGER PRIMARY KEY CHECK(singleton = 1)
);

CREATE TABLE chain_head (
    chain_id TEXT NOT NULL,
    number TEXT NOT NULL,
    hash TEXT NOT NULL,
    time INTEGER NOT NULL,
    observed_at INTEGER NOT NULL,
    singleton INTEGER PRIMARY KEY CHECK(singleton = 1)
);

CREATE TABLE events (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    id TEXT NOT NULL UNIQUE,
    type TEXT NOT NULL,
    payment_id TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    data BLOB
);

CREATE INDEX events_payment_id ON events (payment_id, seq);
CREATE INDEX events_type ON events (type, seq);

CREATE TABLE endpoints (
    id TEXT NOT NULL PRIMARY KEY,
    url TEXT NOT NULL,
    description TEXT NOT NULL,
    events TEXT NOT NULL,
    enabled INTEGER NOT NULL,
    secret TEXT NOT NULL,
    previous_secret TEXT NOT NULL,
    previous_secret_expires_at INTEGER NOT NULL,
    disabled_at INTEGER NOT NULL,
    disabled_reason TEXT NOT NULL,
    failing_since INTEGER NOT NULL,
    consecutive_failures INTEGER NOT NULL,
    last_delivery_at INTEGER NOT NULL,
    last_success_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE INDEX endpoints_created_at ON endpoints (created_at, id);

CREATE TABLE deliveries (
    id TEXT NOT NULL PRIMARY KEY,
    endpoint_id TEXT NOT NULL,
    event_id TEXT NOT NULL,
    event_type TEXT NOT NULL,
    payment_id TEXT NOT NULL,
    url TEXT NOT NULL,
    status TEXT NOT NULL,
    attempt INTEGER NOT NULL,
    max_attempts INTEGER NOT NULL,
    next_attempt_at INTEGER NOT NULL,
    request_body TEXT NOT NULL,
    response_code INTEGER NOT NULL,
    response_body TEXT NOT NULL,
    duration_ms INTEGER NOT NULL,
    error TEXT NOT NULL,
    attempts TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    completed_at INTEGER NOT NULL
);

CREATE INDEX deliveries_created_at ON deliveries (created_at, id);
CREATE INDEX deliveries_next_attempt_at ON deliveries (next_attempt_at, id);
CREATE INDEX deliveries_endpoint_id ON deliveries (endpoint_id);
CREATE INDEX deliveries_payment_id ON deliveries (payment_id);
CREATE INDEX deliveries_event_id ON deliveries (event_id);
CREATE INDEX deliveries_status ON deliveries (status);

CREATE TABLE cursors (consumer TEXT PRIMARY KEY NOT NULL, seq INTEGER NOT NULL);

-- The port permits deliveries for unknown endpoints.
CREATE TRIGGER endpoints_delete AFTER DELETE ON endpoints BEGIN
    DELETE FROM deliveries WHERE endpoint_id = OLD.id;
END;
