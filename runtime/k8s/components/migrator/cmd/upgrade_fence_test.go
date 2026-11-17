package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

func upgradeProtocolFixture(t *testing.T, schema ...int) (*store.DB, string, string, []store.MigrationIdentity) {
	t.Helper()
	ctx := context.Background()
	connection := os.Getenv("AI_SDLC_TEST_DATABASE_URL")
	if connection == "" {
		t.Skip("requires disposable PostgreSQL")
	}
	base, err := store.Open(ctx, connection)
	if err != nil {
		t.Fatal("fixture unavailable")
	}
	t.Cleanup(func() { _ = base.Close() })
	name := "upgrade_protocol_" + strings.ReplaceAll(store.NewID(), "-", "")
	if _, err = base.Pool.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = base.Pool.ExecContext(ctx, "DROP DATABASE "+name+" WITH (FORCE)") })
	u, err := url.Parse(connection)
	if err != nil {
		t.Fatal("fixture connection invalid")
	}
	u.Path = "/" + name
	q := u.Query()
	q.Del("dbname")
	q.Del("database")
	q.Del("search_path")
	u.RawQuery = q.Encode()
	db, err := store.Open(ctx, u.String())
	if err != nil {
		t.Fatal("isolated fixture unavailable")
	}
	t.Cleanup(func() { _ = db.Close() })
	var actual string
	if err = db.Pool.QueryRowContext(ctx, `SELECT current_database()`).Scan(&actual); err != nil || actual != name {
		t.Fatal("fixture database identity changed")
	}
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	source, err := store.MigrationCatalog()
	if err != nil {
		t.Fatal(err)
	}
	count := 34
	if len(schema) > 0 {
		count = schema[0]
	}
	if count != 34 && count != 35 {
		t.Fatal("unsupported fixture schema")
	}
	// Legacy fixtures remain explicitly schema 0034 after the compiler adds 0035.
	if len(source) != 35 {
		t.Fatal("fixture catalog changed")
	}
	if count == 34 {
		if _, err = db.Pool.ExecContext(ctx, `DROP TABLE public.deployment_browser_cleanups,public.deployment_browser_evidence,public.deployment_browser_pods,public.deployment_browser_sends,public.deployment_browser_intents;
 DELETE FROM public.schema_migrations WHERE filename='0035_deployment_browser.sql'`); err != nil {
			t.Fatal(err)
		}
		source = source[:34]
	}
	return db, name, u.String(), source
}

func TestUpgradeFenceProtocolChecksAndReleaseDoNotAdvanceWork(t *testing.T) {
	for _, scenario := range []string{"release", "wrong-nonce", "closed-input", "backend-loss"} {
		t.Run(scenario, func(t *testing.T) {
			db, name, connection, catalog := upgradeProtocolFixture(t)
			input, send := io.Pipe()
			receive, output := io.Pipe()
			defer send.Close()
			defer receive.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- holdUpgradeFenceContext(ctx, input, output) }()
			encoder := json.NewEncoder(send)
			decoder := json.NewDecoder(receive)
			if err := encoder.Encode(map[string]any{"databaseURL": connection, "database": name, "sourceCatalog": catalog}); err != nil {
				t.Fatal("initial frame failed")
			}
			var first map[string]any
			if err := decoder.Decode(&first); err != nil || first["state"] != "held" || first["workflowRowsWritten"] != false || first["upgradeAuthorized"] != false {
				t.Fatal("fence protocol did not acquire only exclusion", err)
			}
			nonce, ok := first["nonce"].(string)
			if !ok || len(nonce) != 36 {
				t.Fatal("original nonce unavailable")
			}
			if err := encoder.Encode(map[string]string{"operation": "check", "nonce": nonce}); err != nil {
				t.Fatal(err)
			}
			var checked map[string]any
			if err := decoder.Decode(&checked); err != nil || checked["expiresAt"] != first["expiresAt"] || checked["nonce"] != nonce || checked["state"] != "held" {
				t.Fatal("check changed original fence", err)
			}
			switch scenario {
			case "release":
				// Repeated protocol checks overlap the 250ms heartbeat while
				// retaining the same nonce and original deadline.
				for n := 0; n < 200; n++ {
					if err := encoder.Encode(map[string]string{"operation": "check", "nonce": nonce}); err != nil {
						t.Fatal("repeated check send failed", err)
					}
					var next map[string]any
					if err := decoder.Decode(&next); err != nil || next["state"] != "held" || next["expiresAt"] != first["expiresAt"] || next["nonce"] != nonce {
						t.Fatal("repeated check lost the original fence", err)
					}
					time.Sleep(3 * time.Millisecond)
				}
				if err := encoder.Encode(map[string]string{"operation": "release", "nonce": nonce}); err != nil {
					t.Fatal(err)
				}
				var released map[string]any
				if err := decoder.Decode(&released); err != nil || released["state"] != "released" || released["upgradeAuthorized"] != false {
					t.Fatal("release acknowledgement missing", err)
				}
			case "wrong-nonce":
				_ = encoder.Encode(map[string]string{"operation": "release", "nonce": store.NewID()})
			case "closed-input":
				_ = send.Close()
			case "backend-loss":
				var killed bool
				if err := db.Pool.QueryRowContext(ctx, `SELECT pg_terminate_backend(pid) FROM pg_locks WHERE locktype='advisory' AND database=(SELECT oid FROM pg_database WHERE datname=current_database()) AND classid=0 AND objid=73421019 AND granted AND pid<>pg_backend_pid()`).Scan(&killed); err != nil || !killed {
					t.Fatal("original fixture backend was not terminated", err)
				}
			}
			select {
			case err := <-done:
				if (scenario == "release") != (err == nil) {
					t.Fatal("unexpected protocol completion", err)
				}
			case <-ctx.Done():
				t.Fatal("protocol did not finish")
			}
			// A normal writer becomes possible only after this original session ends.
			if _, err := db.Pool.ExecContext(ctx, `INSERT INTO public.outbox(id,aggregate_type,aggregate_id,event_type,payload,correlation_id) VALUES($1,'fixture','after','fixture','{}','fixture')`, store.NewID()); err != nil {
				t.Fatal("protocol leaked write exclusion", err)
			}
			var count int
			if err := db.Pool.QueryRowContext(ctx, `SELECT count(*) FROM public.work_items`).Scan(&count); err != nil || count != 0 {
				t.Fatal("administrative protocol advanced delivery", err)
			}
		})
	}
}

func TestUpgradeFenceProtocolHistoryRejectionIsNotTransportOrIdentityFailure(t *testing.T) {
	db, name, connection, catalog := upgradeProtocolFixture(t)
	if _, err := db.Pool.Exec(`INSERT INTO public.release_gates(repository,pull_request,state,correlation_id) VALUES('example/app',1,'blocked','fixture')`); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"history", "wrong-database", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			original := name
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if scenario == "wrong-database" {
				original += "_foreign"
			}
			if scenario == "cancelled" {
				cancel()
			}
			raw, _ := json.Marshal(map[string]any{"databaseURL": connection, "database": original, "sourceCatalog": catalog})
			output := &closedFenceBuffer{}
			err := holdUpgradeFenceContext(ctx, io.NopCloser(bytes.NewReader(append(raw, '\n'))), output)
			if err == nil {
				t.Fatal("negative fence returned success")
			}
			if scenario != "history" {
				if output.Len() != 0 {
					t.Fatal("non-history failure emitted history classification")
				}
				return
			}
			var response map[string]any
			if json.Unmarshal(output.Bytes(), &response) != nil || len(response) != 5 || response["version"] != "ai-sdlc.upgrade-fence/v1" || response["state"] != "rejected" || response["reason"] != "history" || response["upgradeAuthorized"] != false || response["workflowRowsWritten"] != false {
				t.Fatal("explicit compiled history rejection missing")
			}
		})
	}
}

func TestUpgradeFenceProtocolDeadlineClosesBlockedOutput(t *testing.T) {
	db, name, connection, catalog := upgradeProtocolFixture(t)
	input, send := io.Pipe()
	receive, output := io.Pipe()
	defer send.Close()
	defer receive.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- holdUpgradeFenceContext(ctx, input, output) }()
	if err := json.NewEncoder(send).Encode(map[string]any{"databaseURL": connection, "database": name, "sourceCatalog": catalog}); err != nil {
		t.Fatal("initial frame failed")
	}
	// Confirm the actual maintenance session acquired its lock before relying
	// on the timeout as a blocked-output test.
	observedDeadline := time.Now().Add(time.Second)
	for {
		var held bool
		if err := db.Pool.QueryRowContext(context.Background(), `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND database=(SELECT oid FROM pg_database WHERE datname=current_database()) AND classid=0 AND objid=73421019 AND granted)`).Scan(&held); err != nil {
			t.Fatal(err)
		}
		if held {
			break
		}
		if time.Now().After(observedDeadline) {
			t.Fatal("protocol never acquired original maintenance lock")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Do not consume held output. Deadline must close streams and release locks.
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("blocked stream was accepted")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("blocked stream retained the maintenance session")
	}
	probe, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if _, err := db.Pool.ExecContext(probe, `INSERT INTO public.outbox(id,aggregate_type,aggregate_id,event_type,payload,correlation_id) VALUES($1,'fixture','after','fixture','{}','fixture')`, store.NewID()); err != nil {
		t.Fatal("timed out protocol leaked locks", err)
	}
}

type closedFenceBuffer struct{ bytes.Buffer }

func (b *closedFenceBuffer) Close() error { return nil }

func TestUpgradeFenceProtocolRejectsPrivateMalformedFrames(t *testing.T) {
	catalog, err := store.MigrationCatalog()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"database": "ai_sdlc", "databaseURL": "postgres://private-sentinel:secret-sentinel@[invalid", "sourceCatalog": catalog})
	for _, frame := range []string{`{}`, `null`, string(raw), strings.Replace(string(raw), `"database":`, `"Database":`, 1), `{"unknown":"private-sentinel",` + string(raw)[1:], `{"database":"duplicate-private-sentinel",` + string(raw)[1:], strings.Repeat(" ", 131073)} {
		output := &closedFenceBuffer{}
		err := holdUpgradeFence(io.NopCloser(strings.NewReader(frame+"\n")), output)
		if err == nil || err.Error() != "runtime upgrade fence denied" || output.Len() != 0 {
			t.Fatal("malformed frame leaked credentials or emitted custody")
		}
	}
}

func TestUpgradeFenceBrowserProtocolDistinctAndTyped(t *testing.T) {
	for _, count := range []int{34, 35} {
		db, name, connection, catalog := upgradeProtocolFixture(t, count)
		seed, err := os.ReadFile("../../../internal/store/testdata/browser_upgrade_history.sql")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.Pool.Exec(string(seed)); err != nil {
			t.Fatal(err)
		}
		for _, bad := range []bool{false, true, false} {
			if bad {
				if _, err = db.Pool.Exec(`INSERT INTO release_gates(repository,pull_request,state,correlation_id) VALUES('fixture/app',1,'blocked','fixture')`); err != nil {
					t.Fatal(err)
				}
			}
			input, send := io.Pipe()
			receive, output := io.Pipe()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			done := make(chan error, 1)
			go func() { done <- holdSelectedUpgradeFence(ctx, input, output, browserFence) }()
			encoder := json.NewEncoder(send)
			decoder := json.NewDecoder(receive)
			if err = encoder.Encode(map[string]any{"databaseURL": connection, "database": name, "sourceCatalog": catalog}); err != nil {
				t.Fatal(err)
			}
			var first map[string]any
			if err = decoder.Decode(&first); err != nil {
				t.Fatal(err)
			}
			if first["version"] != "ai-sdlc.browser-upgrade-fence/v1" || first["workflowRowsWritten"] != false || first["upgradeAuthorized"] != false {
				t.Fatal("protocol authority mismatch")
			}
			if bad {
				if first["state"] != "rejected" || first["reason"] != "history" || len(first) != 5 {
					t.Fatal("history rejection missing")
				}
				if err = <-done; err == nil {
					t.Fatal("denied history returned success")
				}
			} else {
				if first["state"] != "held" {
					t.Fatal("browser bridge not held")
				}
				for _, operation := range []string{"check", "release"} {
					if err = encoder.Encode(map[string]any{"operation": operation, "nonce": first["nonce"]}); err != nil {
						t.Fatal(err)
					}
					var next map[string]any
					if err = decoder.Decode(&next); err != nil {
						t.Fatal(err)
					}
					state := "held"
					if operation == "release" {
						state = "released"
					}
					if next["state"] != state || next["nonce"] != first["nonce"] || next["expiresAt"] != first["expiresAt"] {
						t.Fatal("original fence changed")
					}
				}
				if err = <-done; err != nil {
					t.Fatal(err)
				}
			}
			cancel()
			send.Close()
			receive.Close()
			if bad {
				if _, err = db.Pool.Exec(`DELETE FROM release_gates`); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}
