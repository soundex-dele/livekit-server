package control

import (
	"context"
	"platform-foundation/server/persistence"
	"testing"
)

func TestAdminMigrationPreservesVersionTwo(t *testing.T) {
	for _, pg := range []bool{false, true} {
		name := "sqlite"
		if pg {
			name = "postgres"
		}
		t.Run(name, func(t *testing.T) {
			s := controlStore(t, pg)
			ctx := context.Background()
			// Reconstruct the exact v2 schema in the isolated fixture, preserving its
			// original checksums and a live business row before applying migration 3.
			for _, query := range []string{"DROP TABLE av_admin_actions", "DROP INDEX av_resources_admin", "DROP INDEX av_resources_platform_admin", "DROP INDEX av_media_operations_admin", "ALTER TABLE av_media_operations DROP COLUMN job_id", "DELETE FROM av_schema WHERE version=3"} {
				if _, e := s.DB.ExecContext(ctx, query); e != nil {
					t.Fatal(e)
				}
			}
			var original string
			if e := s.DB.QueryRowContext(ctx, "SELECT checksum FROM av_schema WHERE version=1").Scan(&original); e != nil {
				t.Fatal(e)
			}
			if e := s.Within(ctx, "org:one", func(tx *persistence.Tx) error {
				return InsertResource(ctx, tx, Resource{ID: "existing", Scope: "org:one", Kind: "meeting", Owner: "owner", Title: "Existing meeting", State: "active", Version: 7})
			}); e != nil {
				t.Fatal(e)
			}
			if e := CheckSchema(ctx, s, ""); e == nil {
				t.Fatal("v2 schema incorrectly accepted")
			}
			for i := 0; i < 2; i++ {
				if e := Migrate(ctx, s, ""); e != nil {
					t.Fatal(e)
				}
			}
			if e := CheckSchema(ctx, s, ""); e != nil {
				t.Fatal(e)
			}
			var checksum, title string
			var version int64
			if e := s.DB.QueryRowContext(ctx, "SELECT checksum FROM av_schema WHERE version=1").Scan(&checksum); e != nil || checksum != original {
				t.Fatal("original checksum changed", e)
			}
			if e := s.DB.QueryRowContext(ctx, "SELECT title,version FROM av_resources WHERE id='existing'").Scan(&title, &version); e != nil || title != "Existing meeting" || version != 7 {
				t.Fatal("data changed", e)
			}
			if _, e := s.DB.ExecContext(ctx, "UPDATE av_schema SET checksum='tampered' WHERE version=3"); e != nil {
				t.Fatal(e)
			}
			if e := CheckSchema(ctx, s, ""); e == nil {
				t.Fatal("tampered appended migration accepted")
			}
		})
	}
}
