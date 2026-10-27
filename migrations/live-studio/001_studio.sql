CREATE TABLE IF NOT EXISTS live_rooms (
 id TEXT PRIMARY KEY, owner TEXT NOT NULL UNIQUE, title TEXT NOT NULL, created INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS live_broadcasts (
 id TEXT PRIMARY KEY, room TEXT NOT NULL, owner TEXT NOT NULL, state TEXT NOT NULL,
 media_room TEXT NOT NULL, stage_room TEXT NOT NULL, created INTEGER NOT NULL,
 heartbeat INTEGER NOT NULL, ended INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS live_one_active ON live_broadcasts(owner) WHERE state != 'ended';
CREATE TABLE IF NOT EXISTS live_presence (
 broadcast TEXT NOT NULL, identity TEXT NOT NULL, user_id TEXT NOT NULL DEFAULT '',
 name TEXT NOT NULL, seen INTEGER NOT NULL, banned INTEGER NOT NULL DEFAULT 0,
 muted INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(broadcast, identity)
);
CREATE TABLE IF NOT EXISTS live_guests (
 broadcast TEXT NOT NULL, user_id TEXT NOT NULL, state TEXT NOT NULL,
 PRIMARY KEY(broadcast,user_id)
);
CREATE TABLE IF NOT EXISTS live_events (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, broadcast TEXT NOT NULL, kind TEXT NOT NULL,
 payload TEXT NOT NULL, created INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS live_events_room ON live_events(broadcast,seq);
CREATE TABLE IF NOT EXISTS live_wallets (
 user_id TEXT NOT NULL, day TEXT NOT NULL, remaining INTEGER NOT NULL CHECK(remaining >= 0),
 PRIMARY KEY(user_id,day)
);
CREATE TABLE IF NOT EXISTS live_gifts (
 id INTEGER PRIMARY KEY AUTOINCREMENT, request_id TEXT NOT NULL, sender TEXT NOT NULL,
 broadcast TEXT NOT NULL, owner TEXT NOT NULL, points INTEGER NOT NULL, day TEXT NOT NULL,
 created INTEGER NOT NULL, UNIQUE(sender,request_id)
);
CREATE TABLE IF NOT EXISTS live_pks (
 id TEXT PRIMARY KEY, a TEXT NOT NULL, b TEXT NOT NULL, state TEXT NOT NULL,
 stage_room TEXT NOT NULL, created INTEGER NOT NULL, started INTEGER NOT NULL DEFAULT 0,
 deadline INTEGER NOT NULL DEFAULT 0, score_a INTEGER NOT NULL DEFAULT 0,
 score_b INTEGER NOT NULL DEFAULT 0, winner TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS live_grants (
 id TEXT PRIMARY KEY, broadcast TEXT NOT NULL, identity TEXT NOT NULL, media_room TEXT NOT NULL,
 user_id TEXT NOT NULL, role TEXT NOT NULL, expires INTEGER NOT NULL, revoked INTEGER NOT NULL DEFAULT 0
);

-- Committed with revocation; retried after failures and service restarts.
CREATE TABLE IF NOT EXISTS live_media_cleanup (
 media_room TEXT NOT NULL, identity TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(media_room,identity)
);
