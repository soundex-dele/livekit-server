package v2

// Schema belongs to the business modules. Foundation and control tables are
// migrated by their owners; no request or service constructor runs DDL.
const Schema = `
CREATE TABLE IF NOT EXISTS av_pairings(code_hash TEXT PRIMARY KEY,scope TEXT NOT NULL,resource_id TEXT NOT NULL,kind TEXT NOT NULL,expires BIGINT NOT NULL,redeemed INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS av_pairings_resource ON av_pairings(scope,resource_id);
CREATE TABLE IF NOT EXISTS av_cast(scope TEXT NOT NULL,id TEXT NOT NULL,sender TEXT NOT NULL DEFAULT '',receiver TEXT NOT NULL,paired BIGINT NOT NULL DEFAULT 0,missing BIGINT NOT NULL DEFAULT 0,PRIMARY KEY(scope,id));
CREATE TABLE IF NOT EXISTS av_assist(scope TEXT NOT NULL,id TEXT NOT NULL,host TEXT NOT NULL,controller TEXT NOT NULL DEFAULT '',controller_name TEXT NOT NULL DEFAULT '',epoch BIGINT NOT NULL DEFAULT 0,missing BIGINT NOT NULL DEFAULT 0,PRIMARY KEY(scope,id));
CREATE TABLE IF NOT EXISTS av_control_grants(id TEXT PRIMARY KEY,scope TEXT NOT NULL,resource_id TEXT NOT NULL,host TEXT NOT NULL,controller TEXT NOT NULL,state TEXT NOT NULL,epoch BIGINT NOT NULL,expires BIGINT NOT NULL,created BIGINT NOT NULL);
CREATE INDEX IF NOT EXISTS av_control_resource ON av_control_grants(scope,resource_id,state);
CREATE TABLE IF NOT EXISTS av_meetings(scope TEXT NOT NULL,id TEXT NOT NULL,number TEXT NOT NULL UNIQUE,host TEXT NOT NULL,starts BIGINT NOT NULL,duration INTEGER NOT NULL,password_salt TEXT NOT NULL,password_hash TEXT NOT NULL,waiting INTEGER NOT NULL,locked INTEGER NOT NULL DEFAULT 0,sharing TEXT NOT NULL DEFAULT '',host_missing BIGINT NOT NULL DEFAULT 0,PRIMARY KEY(scope,id));
CREATE TABLE IF NOT EXISTS av_meeting_members(scope TEXT NOT NULL,resource_id TEXT NOT NULL,subject_id TEXT NOT NULL,name TEXT NOT NULL,unmute_request BIGINT NOT NULL DEFAULT 0,muted INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(scope,resource_id,subject_id));
CREATE TABLE IF NOT EXISTS av_live_rooms(scope TEXT NOT NULL,id TEXT NOT NULL,owner TEXT NOT NULL,current_broadcast TEXT NOT NULL DEFAULT '',PRIMARY KEY(scope,id));
CREATE UNIQUE INDEX IF NOT EXISTS av_live_owner ON av_live_rooms(scope,owner);
CREATE TABLE IF NOT EXISTS av_broadcasts(scope TEXT NOT NULL,id TEXT NOT NULL,room_id TEXT NOT NULL,host TEXT NOT NULL,started BIGINT NOT NULL,pk_id TEXT NOT NULL DEFAULT '',missing BIGINT NOT NULL DEFAULT 0,PRIMARY KEY(scope,id));
CREATE TABLE IF NOT EXISTS av_live_members(scope TEXT NOT NULL,resource_id TEXT NOT NULL,subject_id TEXT NOT NULL,muted INTEGER NOT NULL DEFAULT 0,banned INTEGER NOT NULL DEFAULT 0,stage TEXT NOT NULL DEFAULT '',PRIMARY KEY(scope,resource_id,subject_id));
CREATE TABLE IF NOT EXISTS av_live_chat(id TEXT PRIMARY KEY,scope TEXT NOT NULL,resource_id TEXT NOT NULL,subject_id TEXT NOT NULL,body TEXT NOT NULL,created BIGINT NOT NULL,deleted INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS av_live_chat_resource ON av_live_chat(scope,resource_id,created,id);
CREATE TABLE IF NOT EXISTS av_live_wallets(scope TEXT NOT NULL,subject_id TEXT NOT NULL,day TEXT NOT NULL,balance BIGINT NOT NULL,PRIMARY KEY(scope,subject_id,day));
CREATE TABLE IF NOT EXISTS av_live_gifts(id TEXT PRIMARY KEY,scope TEXT NOT NULL,broadcast_id TEXT NOT NULL,sender TEXT NOT NULL,receiver TEXT NOT NULL,amount BIGINT NOT NULL,day TEXT NOT NULL,created BIGINT NOT NULL);
CREATE INDEX IF NOT EXISTS av_live_gifts_day ON av_live_gifts(scope,day,receiver);
CREATE TABLE IF NOT EXISTS av_live_pk(id TEXT PRIMARY KEY,scope TEXT NOT NULL,left_id TEXT NOT NULL,right_id TEXT NOT NULL,state TEXT NOT NULL,left_score BIGINT NOT NULL DEFAULT 0,right_score BIGINT NOT NULL DEFAULT 0,winner TEXT NOT NULL DEFAULT '',expires BIGINT NOT NULL,created BIGINT NOT NULL);
CREATE INDEX IF NOT EXISTS av_live_pk_active ON av_live_pk(scope,state,expires);
`
