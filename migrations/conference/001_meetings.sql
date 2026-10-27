CREATE TABLE IF NOT EXISTS conference_meetings(id TEXT PRIMARY KEY,number TEXT NOT NULL UNIQUE,data TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS conference_idempotency(actor TEXT NOT NULL,key TEXT NOT NULL,request TEXT NOT NULL,response TEXT NOT NULL,PRIMARY KEY(actor,key));
CREATE TABLE IF NOT EXISTS conference_audit(id INTEGER PRIMARY KEY,meeting_id TEXT,actor TEXT,action TEXT,created INTEGER);
