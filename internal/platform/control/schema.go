package control

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"platform-foundation/server/persistence"
	"strings"
)

const Schema = `
CREATE TABLE av_resources(id TEXT PRIMARY KEY,scope TEXT NOT NULL,kind TEXT NOT NULL,owner TEXT NOT NULL,title TEXT NOT NULL,state TEXT NOT NULL,version BIGINT NOT NULL,created BIGINT NOT NULL,updated BIGINT NOT NULL);
CREATE INDEX av_resources_list ON av_resources(scope,kind,owner,state,created);
CREATE TABLE av_participants(scope TEXT NOT NULL,resource_id TEXT NOT NULL,subject_id TEXT NOT NULL,role TEXT NOT NULL,state TEXT NOT NULL,policy_version BIGINT NOT NULL,updated BIGINT NOT NULL,PRIMARY KEY(scope,resource_id,subject_id));
CREATE INDEX av_participants_subject ON av_participants(scope,subject_id,state);
CREATE TABLE av_credentials(hash TEXT PRIMARY KEY,scope TEXT NOT NULL,resource_id TEXT NOT NULL,subject_id TEXT NOT NULL,role TEXT NOT NULL,expires BIGINT NOT NULL,revoked INTEGER NOT NULL DEFAULT 0);
CREATE TABLE av_idempotency(scope TEXT NOT NULL,subject_id TEXT NOT NULL,action TEXT NOT NULL,key TEXT NOT NULL,hash TEXT NOT NULL,response TEXT NOT NULL,created BIGINT NOT NULL,PRIMARY KEY(scope,subject_id,action,key));
CREATE TABLE av_event_heads(scope TEXT NOT NULL,resource_id TEXT NOT NULL,sequence BIGINT NOT NULL,PRIMARY KEY(scope,resource_id));
CREATE TABLE av_events(id TEXT PRIMARY KEY,scope TEXT NOT NULL,resource_id TEXT NOT NULL,sequence BIGINT NOT NULL,type TEXT NOT NULL,payload TEXT NOT NULL,created BIGINT NOT NULL,UNIQUE(scope,resource_id,sequence));
CREATE INDEX av_events_resource ON av_events(scope,resource_id,sequence);
CREATE TABLE av_media_nodes(id TEXT PRIMARY KEY,incarnation TEXT NOT NULL,enabled INTEGER NOT NULL,healthy INTEGER NOT NULL,capacity INTEGER NOT NULL,updated BIGINT NOT NULL);
CREATE TABLE av_room_bindings(id TEXT PRIMARY KEY,scope TEXT NOT NULL,resource_id TEXT NOT NULL,purpose TEXT NOT NULL,node_id TEXT NOT NULL,node_incarnation TEXT NOT NULL,room TEXT NOT NULL UNIQUE,generation BIGINT NOT NULL,revision BIGINT NOT NULL,state TEXT NOT NULL,created BIGINT NOT NULL,UNIQUE(scope,resource_id,purpose,generation));
CREATE INDEX av_binding_node ON av_room_bindings(node_id,state);
CREATE TABLE av_rtc_grants(id TEXT PRIMARY KEY,scope TEXT NOT NULL,resource_id TEXT NOT NULL,subject_id TEXT NOT NULL,role TEXT NOT NULL,binding_id TEXT NOT NULL,identity TEXT NOT NULL UNIQUE,policy_version BIGINT NOT NULL,capabilities TEXT NOT NULL,status TEXT NOT NULL,expires BIGINT NOT NULL,created BIGINT NOT NULL);
CREATE INDEX av_grants_resource ON av_rtc_grants(scope,resource_id,subject_id,status);
CREATE TABLE av_media_operations(id TEXT PRIMARY KEY,scope TEXT NOT NULL,resource_id TEXT NOT NULL,binding_id TEXT NOT NULL,subject_id TEXT NOT NULL,action TEXT NOT NULL,revision BIGINT NOT NULL,payload TEXT NOT NULL,state TEXT NOT NULL,created BIGINT NOT NULL,updated BIGINT NOT NULL);
CREATE INDEX av_media_operations_pending ON av_media_operations(state,updated);
`

// Migrate is explicit and checksum-checked. Startup only calls CheckSchema.
func Migrate(ctx context.Context, s *persistence.Store, businessSchema string) error {
	if e := s.Migrate(ctx); e != nil {
		return e
	}
	if _, e := s.DB.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS av_schema(version INTEGER PRIMARY KEY,checksum TEXT NOT NULL)"); e != nil {
		return e
	}
	return s.Within(ctx, "app", func(t *persistence.Tx) error {
		for i, ddl := range []string{Schema, businessSchema} {
			checksum := fmt.Sprintf("%x", sha256.Sum256([]byte(ddl)))
			var got string
			e := t.Row(ctx, "SELECT checksum FROM av_schema WHERE version=?", i+1).Scan(&got)
			if e == nil {
				if got != checksum {
					return fmt.Errorf("AV migration %d checksum mismatch", i+1)
				}
				continue
			}
			if !errors.Is(e, sql.ErrNoRows) {
				return e
			}
			for _, statement := range strings.Split(ddl, ";") {
				if strings.TrimSpace(statement) == "" {
					continue
				}
				if _, e = t.Exec(ctx, statement); e != nil {
					return fmt.Errorf("AV migration %d: %w", i+1, e)
				}
			}
			if _, e = t.Exec(ctx, "INSERT INTO av_schema(version,checksum) VALUES(?,?)", i+1, checksum); e != nil {
				return e
			}
		}
		return nil
	})
}
func CheckSchema(ctx context.Context, s *persistence.Store, business ...string) error {
	var n int
	if e := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM av_schema").Scan(&n); e != nil || n != 2 {
		return fmt.Errorf("run business-api migrate before startup")
	}
	steps := []string{Schema}
	if len(business) > 0 {
		steps = append(steps, business[0])
	}
	for i, ddl := range steps {
		var got string
		if e := s.DB.QueryRowContext(ctx, s.Q("SELECT checksum FROM av_schema WHERE version=?"), i+1).Scan(&got); e != nil {
			return e
		}
		if got != fmt.Sprintf("%x", sha256.Sum256([]byte(ddl))) {
			return fmt.Errorf("AV migration %d checksum mismatch", i+1)
		}
	}
	return nil
}
