package execution

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/provenance"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

// This fixture isolates SQL selection using copied table types/checks/indexes in
// a disposable schema. Its older row is an intentionally incomplete historical
// projection, never signed authority. No claim or runtime executes that row.
func TestProvenanceSelectorsSkipOlderUnusableProjection(t *testing.T) {
	for _, kind := range []string{"stale-issue", "legacy-context"} {
		t.Run(kind, func(t *testing.T) {
			p := newProvenanceFixture(t)
			ctx := context.Background()
			provenanceCleanupFixture(t, p)
			if e := p.j.QueueProvenance(ctx, p.payload, p.receipt); e != nil {
				t.Fatal(e)
			}
			schema := "selector_" + strings.ReplaceAll(store.NewID(), "-", "")
			if _, e := p.b.f.Admin.Pool.Exec(`CREATE SCHEMA ` + schema); e != nil {
				t.Fatal(e)
			}
			t.Cleanup(func() { _, _ = p.b.f.Admin.Pool.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })
			tables := []string{"provenance_requests", "provenance_handoffs", "build_requests", "built_records", "build_contexts", "build_cleanups", "merge_requests", "publication_requests", "work_items", "intake_heads", "intake_revisions", "intake_observations", "intake_cursors"}
			for _, table := range tables {
				if _, e := p.b.f.Admin.Pool.Exec(`CREATE TABLE ` + schema + `.` + table + ` (LIKE public.` + table + ` INCLUDING ALL)`); e != nil {
					t.Fatal(e)
				}
				if _, e := p.b.f.Admin.Pool.Exec(`INSERT INTO ` + schema + `.` + table + ` SELECT * FROM public.` + table); e != nil {
					t.Fatal(e)
				}
				if _, e := p.b.f.Admin.Pool.Exec(`GRANT SELECT ON ` + schema + `.` + table + ` TO aisdlc_verifier,aisdlc_build_controller`); e != nil {
					t.Fatal(e)
				}
			}
			if _, e := p.b.f.Admin.Pool.Exec(`GRANT USAGE ON SCHEMA ` + schema + ` TO aisdlc_verifier,aisdlc_build_controller`); e != nil {
				t.Fatal(e)
			}
			clone := func(table string, patch map[string]any) {
				t.Helper()
				raw, e := json.Marshal(patch)
				if e != nil {
					t.Fatal(e)
				}
				q := `INSERT INTO ` + schema + `.` + table + ` SELECT (jsonb_populate_record(NULL::` + schema + `.` + table + `,to_jsonb(original)||$1::jsonb)).* FROM public.` + table + ` original LIMIT 1`
				if _, e = p.b.f.Admin.Pool.Exec(q, string(raw)); e != nil {
					t.Fatal("projection", table, e)
				}
			}
			buildID := "build-" + strings.Repeat("0", 64)
			mergeID := "merge-" + strings.Repeat("0", 64)
			pubID := "publication-" + strings.Repeat("0", 64)
			requestID := "verify-" + strings.Repeat("0", 64)
			clone("publication_requests", map[string]any{"id": pubID, "review_id": store.NewID(), "verification_id": "verification-" + strings.Repeat("0", 64), "candidate_id": "execution-" + strings.Repeat("0", 64)})
			clone("merge_requests", map[string]any{"id": mergeID, "publication_id": pubID})
			build := p.record.Command
			if kind == "stale-issue" {
				build.IssueNumber = 9999
			} // no current head for withdrawn input
			raw, _ := json.Marshal(build)
			clone("build_requests", map[string]any{"id": buildID, "merge_id": mergeID, "payload": "\\x" + hex.EncodeToString(raw)})
			clone("built_records", map[string]any{"request_id": buildID, "recorded_at": p.record.RecordedAt.Add(-time.Second)})
			clone("build_cleanups", map[string]any{"request_id": buildID})
			// No retained context for A: legacy selection must not monopolize dispatch.
			command := p.command
			command.BuildID = buildID
			raw, _ = json.Marshal(command)
			clone("provenance_requests", map[string]any{"id": requestID, "build_id": buildID, "payload": "\\x" + hex.EncodeToString(raw), "created_at": time.Now().Add(-time.Minute)})
			role := "verifier"
			if kind == "legacy-context" {
				role = "build-controller"
			}
			u, e := url.Parse(os.Getenv("AI_SDLC_TEST_DATABASE_URL"))
			if e != nil {
				t.Fatal(e)
			}
			roleName, _ := store.ComponentRole(role)
			u.User = url.UserPassword(roleName, p.b.passwords[role])
			q := u.Query()
			q.Set("search_path", schema)
			u.RawQuery = q.Encode()
			db, e := store.Open(ctx, u.String())
			if e != nil {
				t.Fatal(e)
			}
			defer db.Close()
			var selected string
			if kind == "stale-issue" {
				j := store.NewProvenanceJournal(db, func() (provenance.Configuration, error) { return *p.cfg, nil })
				selected, e = j.NextProvenance(ctx)
				if selected != p.command.ID() {
					t.Fatal("older stale queue hid valid request", selected, e)
				}
			} else {
				selected, e = db.NextBuiltForProvenance(ctx, *p.cfg)
				if selected != p.record.Command.ID() {
					t.Fatal("older legacy history hid retained build", selected, e)
				}
			}
			if e != nil {
				t.Fatal(e)
			}
		})
	}
}
