-- Durable values owned by adapters rather than by the core: one opaque blob
-- per key, replaced atomically (the chain watcher's checkpoint). Outside
-- the core.Store port on purpose. updated_at is in Unix milliseconds.
CREATE TABLE kv (
	key        TEXT PRIMARY KEY NOT NULL,
	value      BLOB NOT NULL,
	updated_at INTEGER NOT NULL
) WITHOUT ROWID;
