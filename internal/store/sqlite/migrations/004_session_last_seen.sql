-- Session idle timeout (core.Session.LastSeenAt): when a session last
-- authenticated a request. The default is the millisecond encoding of the
-- zero time; the core counts such sessions as last used when they were
-- created.
ALTER TABLE sessions ADD COLUMN last_seen_at INTEGER NOT NULL DEFAULT -62135596800000;
