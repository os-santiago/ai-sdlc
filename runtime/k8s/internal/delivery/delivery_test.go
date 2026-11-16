package delivery

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

type fixture struct {
	p         authority.Policy
	ids       map[string]authority.Identity
	admin     *store.DB
	dbs       map[string]*store.DB
	passwords map[string]string
}

func setup(t *testing.T) fixture {
	t.Helper()
	databaseURL := os.Getenv("AI_SDLC_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("AI_SDLC_TEST_DATABASE_URL is required for signed delivery integration tests")
	}
	ctx := context.Background()
	admin, err := store.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Pool.ExecContext(ctx, `TRUNCATE authorized_outbox,authorized_inbox,authorized_nonces`); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Pool.ExecContext(ctx, `DELETE FROM admissions WHERE repository='example/project'`); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../../../spec/fixtures/component-authority.v1.json")
	if err != nil {
		t.Fatal(err)
	}
	p, err := authority.ParsePolicy(raw)
	if err != nil {
		t.Fatal(err)
	}
	f := fixture{p: p, admin: admin, ids: map[string]authority.Identity{}, dbs: map[string]*store.DB{}, passwords: map[string]string{}}
	f.p.Keys = map[string]authority.PublicKey{}
	f.p.Autonomy, f.p.Freeze = "auto-deploy", false
	for _, role := range []string{"intake", "admission-controller", "orchestrator", "worker", "verifier", "release-manager", "relay", "deployer", "build-controller"} {
		i, key, err := authority.NewIdentity(role, role+"-1")
		if err != nil {
			t.Fatal(err)
		}
		f.ids[role], f.p.Keys[i.KeyID] = i, key
		f.passwords[role] = strings.ReplaceAll(role, "-", "_") + strings.Repeat("x", 40)
	}
	if err := admin.ProvisionRoles(ctx, f.passwords); err != nil {
		t.Fatal(err)
	}
	for role, password := range f.passwords {
		u, err := url.Parse(databaseURL)
		if err != nil {
			t.Fatal(err)
		}
		name, _ := store.ComponentRole(role)
		u.User = url.UserPassword(name, password)
		db, err := store.Open(ctx, u.String())
		if err != nil {
			t.Fatalf("connect component %s", role)
		}
		f.dbs[role] = db
		t.Cleanup(func() { _ = db.Close() })
	}
	return f
}

func (f fixture) envelope(t *testing.T, id string, payload []byte) Envelope {
	t.Helper()
	r, err := f.ids["intake"].Seal(f.p, id, "admit-issue", payload)
	if err != nil {
		t.Fatal(err)
	}
	transport, err := f.ids["relay"].Carry(f.p, r, http.MethodPost, "/admit", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return Envelope{Receipt: r, Transport: transport, Payload: payload}
}

func persistAdmission(ctx context.Context, tx *sql.Tx, r authority.Receipt, payload []byte) ([]byte, error) {
	_, err := tx.ExecContext(ctx, `INSERT INTO admissions(repository,issue_number,issue_revision,decision,risk,policy_version,correlation_id)
		VALUES($1,1,$2,'accepted','low','1',$2)`, r.Repository, r.ID)
	return []byte(`{"accepted":true}`), err
}

func count(t *testing.T, db *store.DB, query string) int {
	t.Helper()
	var n int
	if err := db.Pool.QueryRowContext(context.Background(), query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestConcurrentRedeliveryCommitsOneEffect(t *testing.T) {
	f := setup(t)
	in := Inbox{DB: f.dbs["admission-controller"], Target: "admission-controller"}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var calls atomic.Int32
	effect := func(ctx context.Context, tx *sql.Tx, r authority.Receipt, payload []byte) ([]byte, error) {
		calls.Add(1)
		return persistAdmission(ctx, tx, r, payload)
	}
	results := make(chan error, 20)
	for range 20 {
		envelope := f.envelope(t, "event-concurrent", []byte(`{"issue":1}`))
		go func() {
			result, _, err := in.Accept(ctx, f.p, envelope, "admit-issue", "POST", "/admit", time.Now(), effect)
			if err == nil && string(result) != `{"accepted":true}` {
				err = errors.New("stored response mismatch")
			}
			results <- err
		}()
	}
	for range 20 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 || count(t, f.admin, `SELECT count(*) FROM authorized_inbox`) != 1 {
		t.Fatal("duplicate durable effect")
	}
	// A new pool simulates process restart: deduplication is not a memory cache.
	u, _ := url.Parse(os.Getenv("AI_SDLC_TEST_DATABASE_URL"))
	u.User = url.UserPassword("aisdlc_admission_controller", f.passwords["admission-controller"])
	restarted, err := store.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	in.DB = restarted
	_, duplicate, err := in.Accept(ctx, f.p, f.envelope(t, "event-concurrent", []byte(`{"issue":1}`)), "admit-issue", "POST", "/admit", time.Now(), effect)
	if err != nil || !duplicate || calls.Load() != 1 {
		t.Fatalf("restart deduplication failed: %v", err)
	}
}

func TestReplayConflictAndRollback(t *testing.T) {
	f := setup(t)
	in := Inbox{DB: f.dbs["admission-controller"], Target: "admission-controller"}
	ctx := context.Background()
	envelope := f.envelope(t, "event-rollback", []byte(`{"issue":1}`))
	fail := func(ctx context.Context, tx *sql.Tx, r authority.Receipt, payload []byte) ([]byte, error) {
		if _, err := persistAdmission(ctx, tx, r, payload); err != nil {
			return nil, err
		}
		if err := Append(ctx, tx, f.p, f.ids["admission-controller"], "admission-result", "plan-admission", payload); err != nil {
			return nil, err
		}
		return nil, errors.New("simulated interruption before commit")
	}
	if _, _, err := in.Accept(ctx, f.p, envelope, "admit-issue", "POST", "/admit", time.Now(), fail); err == nil {
		t.Fatal("expected interruption")
	}
	for _, table := range []string{"authorized_inbox", "authorized_outbox", "authorized_nonces", "admissions"} {
		query := "SELECT count(*) FROM " + table
		if table == "admissions" {
			query += " WHERE repository='example/project'"
		}
		if count(t, f.admin, query) != 0 {
			t.Fatalf("partial commit in %s", table)
		}
	}
	if _, _, err := in.Accept(ctx, f.p, envelope, "admit-issue", "POST", "/admit", time.Now(), persistAdmission); err != nil {
		t.Fatal(err)
	}
	if _, _, err := in.Accept(ctx, f.p, envelope, "admit-issue", "POST", "/admit", time.Now(), persistAdmission); !errors.Is(err, ErrReplay) {
		t.Fatalf("replayed transport: %v", err)
	}
	conflict := f.envelope(t, "event-rollback", []byte(`{"issue":2}`))
	if _, _, err := in.Accept(ctx, f.p, conflict, "admit-issue", "POST", "/admit", time.Now(), persistAdmission); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed event: %v", err)
	}
	if count(t, f.admin, `SELECT count(*) FROM admissions WHERE repository='example/project'`) != 1 {
		t.Fatal("conflicting event applied")
	}
}

func TestDatabaseOwnershipAndProvisioning(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	for _, test := range []struct{ role, query string }{
		{"intake", `INSERT INTO authorized_outbox(producer,event_id,target,operation,receipt,payload) VALUES('worker','forged','orchestrator','record-attempt','{}','x')`},
		{"intake", `UPDATE authorized_outbox SET payload='modified'`},
		{"intake", `INSERT INTO authorized_outbox(producer,event_id,target,operation,receipt,payload,attempts) VALUES('intake','poison','admission-controller','admit-issue','{}','x',8)`},
		{"intake", `INSERT INTO authorized_outbox(producer,event_id,target,operation,receipt,payload,published_at) VALUES('intake','forged-ack','admission-controller','admit-issue','{}','x',now())`},

		{"relay", `UPDATE authorized_outbox SET receipt='{}'`},
		{"relay", `INSERT INTO authorized_outbox(producer,event_id,target,operation,receipt,payload) VALUES('relay','forged','worker','execute-work','{}','x')`},
		{"relay", `SELECT * FROM authorized_inbox`},
		{"worker", `UPDATE admissions SET decision='accepted'`},
		{"worker", `CREATE TABLE privilege_escalation(value text)`},
		{"worker", `SET ROLE aisdlc_release_manager`},
		{"admission-controller", `INSERT INTO authorized_inbox(target,producer,event_id,receipt_digest) VALUES('orchestrator','intake','forged','x')`},
	} {
		if _, err := f.dbs[test.role].Pool.ExecContext(ctx, test.query); err == nil {
			t.Fatalf("database allowed %s: %s", test.role, test.query)
		}
	}
	err := f.admin.WithTx(ctx, func(tx *sql.Tx) error {
		return Append(ctx, tx, f.p, f.ids["intake"], "admin-forgery", "admit-issue", []byte(`{}`))
	})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("bootstrap identity used operationally: %v", err)
	}
	if _, err := f.admin.Pool.ExecContext(ctx, `GRANT UPDATE(payload) ON authorized_outbox TO aisdlc_relay; GRANT UPDATE(receipt) ON authorized_outbox TO PUBLIC; ALTER ROLE aisdlc_worker BYPASSRLS`); err != nil {
		t.Fatal(err)
	}
	if err := store.AssertComponent(ctx, f.dbs["worker"].Pool, "worker"); err == nil {
		t.Fatal("elevated component identity accepted")
	}
	if err := f.admin.ProvisionRoles(ctx, f.passwords); err != nil {
		t.Fatalf("repeat provisioning: %v", err)
	}
	for _, column := range []string{"payload", "receipt"} {
		if _, err := f.dbs["relay"].Pool.ExecContext(ctx, "UPDATE authorized_outbox SET "+column+"="+column); err == nil {
			t.Fatalf("column grant drift preserved: %s", column)
		}
	}
	if err := store.AssertComponent(ctx, f.dbs["worker"].Pool, "worker"); err != nil {
		t.Fatal("reconciled component identity rejected")
	}

	if _, err := f.admin.Pool.ExecContext(ctx, `DELETE FROM managed_component_roles WHERE component='worker'`); err != nil {
		t.Fatal(err)
	}
	if err := f.admin.ProvisionRoles(ctx, f.passwords); err == nil {
		t.Fatal("unmanaged role adopted")
	}
	if _, err := f.admin.Pool.ExecContext(ctx, `INSERT INTO managed_component_roles(component,role_name) VALUES('worker','aisdlc_worker')`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.Pool.ExecContext(ctx, `GRANT aisdlc_release_manager TO aisdlc_worker`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.admin.Pool.ExecContext(context.Background(), `REVOKE aisdlc_release_manager FROM aisdlc_worker`)
	})
	if err := f.admin.ProvisionRoles(ctx, f.passwords); err == nil {
		t.Fatal("settable role membership accepted")
	}
}

func TestOutboxImmutableAndScopeDenials(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	appendEvent := func(payload string) error {
		return f.dbs["intake"].WithTx(ctx, func(tx *sql.Tx) error {
			return Append(ctx, tx, f.p, f.ids["intake"], "immutable", "admit-issue", []byte(payload))
		})
	}
	if err := appendEvent(`{ "issue": 1 }`); err != nil {
		t.Fatal(err)
	}
	if err := appendEvent(`{ "issue": 1 }`); err != nil {
		t.Fatal(err)
	}
	if err := appendEvent(`{"issue":2}`); !errors.Is(err, ErrConflict) {
		t.Fatalf("immutable event changed: %v", err)
	}
	if count(t, f.admin, `SELECT count(*) FROM authorized_outbox`) != 1 {
		t.Fatal("duplicate outbox fact")
	}
	in := Inbox{DB: f.dbs["admission-controller"], Target: "admission-controller"}
	for _, scenario := range []string{"freeze", "repository", "environment", "signature", "payload"} {
		envelope := f.envelope(t, "denied-"+scenario, []byte(`{}`))
		p := f.p
		switch scenario {
		case "freeze":
			p.Freeze = true
		case "repository":
			p.Repository = "other/project"
		case "environment":
			p.Environment = "production"
		case "signature":
			envelope.Receipt.Signature = "invalid"
		case "payload":
			envelope.Payload = []byte(`{"changed":true}`)
		}
		if _, _, err := in.Accept(ctx, p, envelope, "admit-issue", "POST", "/admit", time.Now(), persistAdmission); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("%s accepted: %v", scenario, err)
		}
	}
	if count(t, f.admin, `SELECT count(*) FROM authorized_inbox`) != 0 {
		t.Fatal("denied event wrote inbox")
	}
}

func TestRelayRecoversLostAcknowledgementWithoutRepeatingEffect(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	in := Inbox{DB: f.dbs["admission-controller"], Target: "admission-controller"}
	var calls, requests atomic.Int32
	payload := []byte("{  \"issue\" : 1 }\n")
	effect := func(ctx context.Context, tx *sql.Tx, r authority.Receipt, raw []byte) ([]byte, error) {
		calls.Add(1)
		if string(raw) != string(payload) {
			return nil, errors.New("signed payload bytes changed")
		}
		return persistAdmission(ctx, tx, r, raw)
	}
	handler := in.Handler(func() (authority.Policy, authority.Identity, error) { return f.p, f.ids["admission-controller"], nil }, "admit-issue", "/admit", effect)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, r)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	if err := f.dbs["intake"].WithTx(ctx, func(tx *sql.Tx) error {
		return Append(ctx, tx, f.p, f.ids["intake"], "lost-ack", "admit-issue", payload)
	}); err != nil {
		t.Fatal(err)
	}
	relay := Relay{DB: f.dbs["relay"], Identity: f.ids["relay"], Routes: map[string]string{"admit-issue": server.URL + "/admit"}}
	now := time.Now()
	if delivered, err := relay.PollOnce(ctx, f.p, now); err != nil || delivered {
		t.Fatalf("first unacknowledged request: %v", err)
	}
	if delivered, err := relay.PollOnce(ctx, f.p, now.Add(4*time.Second)); err != nil || !delivered {
		t.Fatalf("redelivery: %v", err)
	}
	if calls.Load() != 1 || requests.Load() != 2 {
		t.Fatal("redelivery repeated effect")
	}
	if count(t, f.admin, `SELECT count(*) FROM authorized_outbox WHERE published_at IS NOT NULL AND attempts=2`) != 1 {
		t.Fatal("delivery not committed")
	}
}

func TestRelayRejectsPlainOKAndBoundsRetries(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); _, _ = w.Write([]byte(`{"ok":true}`)) }))
	defer server.Close()
	if err := f.dbs["intake"].WithTx(ctx, func(tx *sql.Tx) error {
		return Append(ctx, tx, f.p, f.ids["intake"], "bounded-retries", "admit-issue", []byte(`{}`))
	}); err != nil {
		t.Fatal(err)
	}
	relay := Relay{DB: f.dbs["relay"], Identity: f.ids["relay"], Routes: map[string]string{"admit-issue": server.URL + "/admit"}}
	now := time.Now()
	frozen := f.p
	frozen.Freeze = true
	if _, err := relay.PollOnce(ctx, frozen, now); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("frozen relay polled")
	}
	for i := 0; i < 10; i++ {
		if delivered, err := relay.PollOnce(ctx, f.p, now.Add(time.Duration(i)*time.Hour)); err != nil || delivered {
			t.Fatalf("unsigned acknowledgement accepted: %v", err)
		}
	}
	if calls.Load() != 8 || count(t, f.admin, `SELECT count(*) FROM authorized_outbox WHERE exhausted AND attempts=8 AND published_at IS NULL`) != 1 {
		t.Fatal("unbounded retries or dropped event")
	}
}

func TestRelayDoesNotFollowRedirect(t *testing.T) {
	f := setup(t)
	var calls atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	envelope := f.envelope(t, "redirect", []byte(`{}`))
	relay := Relay{Identity: f.ids["relay"], Routes: map[string]string{"admit-issue": redirect.URL + "/admit"}}
	if got := relay.send(context.Background(), f.p, envelope.Receipt, envelope.Payload, time.Now()); got != "receiver_rejected" || calls.Load() != 0 {
		t.Fatal("redirect forwarded signed payload")
	}
}

func TestHTTPRejectsMalformedInputBeforeEffect(t *testing.T) {
	f := setup(t)
	in := Inbox{DB: f.dbs["admission-controller"], Target: "admission-controller"}
	handler := in.Handler(func() (authority.Policy, authority.Identity, error) { return f.p, f.ids["admission-controller"], nil }, "admit-issue", "/admit", persistAdmission)
	for _, body := range []string{`{}`, `{} {}`, `{"payload":null,"payload":"e30="}`, strings.Repeat(" ", 2*1024*1024+1)} {
		r := httptest.NewRequest(http.MethodPost, "/admit", strings.NewReader(body))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code < 400 {
			t.Fatal("malformed envelope accepted")
		}
	}
	if count(t, f.admin, `SELECT count(*) FROM authorized_inbox`) != 0 {
		t.Fatal("malformed input persisted")
	}
}

func TestNonceRetainedDuringFinalAcceptedSecond(t *testing.T) {
	f := setup(t)
	in := Inbox{DB: f.dbs["admission-controller"], Target: "admission-controller"}
	envelope := f.envelope(t, "nonce-boundary", []byte(`{}`))
	now := time.Unix(envelope.Transport.IssuedAt, 0)
	if _, _, err := in.Accept(context.Background(), f.p, envelope, "admit-issue", "POST", "/admit", now, persistAdmission); err != nil {
		t.Fatal(err)
	}
	finalSecond := time.Unix(envelope.Transport.IssuedAt+300, 500000000)
	if _, _, err := in.Accept(context.Background(), f.p, envelope, "admit-issue", "POST", "/admit", finalSecond, persistAdmission); !errors.Is(err, ErrReplay) {
		t.Fatalf("nonce lost before transport expiry: %v", err)
	}
	if count(t, f.admin, `SELECT count(*) FROM authorized_nonces`) != 1 {
		t.Fatal("live nonce removed")
	}
}

func TestInvalidReceiverIdentityHasNoEffect(t *testing.T) {
	f := setup(t)
	in := Inbox{DB: f.dbs["admission-controller"], Target: "admission-controller"}
	raw, err := json.Marshal(f.envelope(t, "invalid-receiver", []byte(`{}`)))
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"missing-key", "wrong-private-key", "unknown-key-id", "wrong-role"} {
		identity := f.ids["admission-controller"]
		switch scenario {
		case "missing-key":
			identity.PrivateKey = ""
		case "wrong-private-key":
			identity.PrivateKey = f.ids["worker"].PrivateKey
		case "unknown-key-id":
			identity.KeyID = "unregistered"
		case "wrong-role":
			identity = f.ids["worker"]
		}
		handler := in.Handler(func() (authority.Policy, authority.Identity, error) { return f.p, identity, nil }, "admit-issue", "/admit", persistAdmission)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/admit", bytes.NewReader(raw)))
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s returned %d", scenario, w.Code)
		}
	}
	for _, table := range []string{"authorized_inbox", "authorized_nonces"} {
		if count(t, f.admin, "SELECT count(*) FROM "+table) != 0 {
			t.Fatalf("invalid receiver wrote %s", table)
		}
	}
	if count(t, f.admin, `SELECT count(*) FROM admissions WHERE repository='example/project'`) != 0 {
		t.Fatal("invalid receiver committed an effect")
	}
}

func TestInvalidRelayIdentityDoesNotConsumeRetries(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if err := f.dbs["intake"].WithTx(ctx, func(tx *sql.Tx) error {
		return Append(ctx, tx, f.p, f.ids["intake"], "bad-relay-key", "admit-issue", []byte(`{}`))
	}); err != nil {
		t.Fatal(err)
	}
	identity := f.ids["relay"]
	identity.PrivateKey = f.ids["worker"].PrivateKey
	relay := Relay{DB: f.dbs["relay"], Identity: identity}
	if _, err := relay.PollOnce(ctx, f.p, time.Now()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("bad relay identity accepted: %v", err)
	}
	if count(t, f.admin, `SELECT count(*) FROM authorized_outbox WHERE attempts=0 AND published_at IS NULL`) != 1 {
		t.Fatal("invalid relay consumed a delivery attempt")
	}
}

func TestBootstrapCannotBecomeOperationalWithSetRole(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	err := f.admin.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `SET LOCAL ROLE aisdlc_worker`); err != nil {
			return err
		}
		return store.AssertComponent(ctx, tx, "worker")
	})
	if err == nil {
		t.Fatal("bootstrap session impersonated operational login")
	}
	if err := store.AssertComponent(ctx, f.dbs["worker"].Pool, "worker"); err != nil {
		t.Fatal("dedicated login rejected")
	}
}

func TestReservationSurvivesCrashAndStaleOwnerCannotFinalize(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if err := f.dbs["intake"].WithTx(ctx, func(tx *sql.Tx) error {
		return Append(ctx, tx, f.p, f.ids["intake"], "crashed-relay", "admit-issue", []byte(`{}`))
	}); err != nil {
		t.Fatal(err)
	}
	relay := Relay{DB: f.dbs["relay"], Identity: f.ids["relay"]}
	now := time.Now()
	old, err := relay.reserve(ctx, f.p, now)
	if err != nil || old == nil {
		t.Fatalf("reservation failed: %v", err)
	}
	if count(t, f.admin, `SELECT count(*) FROM authorized_outbox WHERE attempts=1 AND lease_token IS NOT NULL`) != 1 {
		t.Fatal("reservation not committed before I/O")
	}
	if row, err := relay.reserve(ctx, f.p, now.Add(time.Second)); err != nil || row != nil {
		t.Fatalf("active lease stolen: %v", err)
	}
	restarted := Relay{DB: f.dbs["relay"], Identity: f.ids["relay"]}
	next, err := restarted.reserve(ctx, f.p, now.Add(31*time.Second))
	if err != nil || next == nil || next.attempts != 2 {
		t.Fatalf("restart lost attempt budget: %v", err)
	}
	if done, err := relay.finish(ctx, *old, "", now.Add(32*time.Second)); err != nil || done {
		t.Fatalf("stale owner finalized: %v", err)
	}
	if count(t, f.admin, `SELECT count(*) FROM authorized_outbox WHERE attempts=2 AND published_at IS NULL`) != 1 {
		t.Fatal("stale lease changed delivery state")
	}
	for attempt := 3; attempt <= 8; attempt++ {
		row, err := restarted.reserve(ctx, f.p, now.Add(time.Duration(attempt-1)*31*time.Second))
		if err != nil || row == nil || row.attempts != attempt {
			t.Fatalf("reservation %d: %v", attempt, err)
		}
	}
	if row, err := restarted.reserve(ctx, f.p, now.Add(time.Hour)); err != nil || row != nil {
		t.Fatalf("ninth reservation after crash: %v", err)
	}
	if count(t, f.admin, `SELECT count(*) FROM authorized_outbox WHERE attempts=8 AND exhausted AND published_at IS NULL`) != 1 {
		t.Fatal("crashes bypassed retry budget")
	}
}

func TestCrashAfterCommittedReceiverRecoversWithFreshLease(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	var effects atomic.Int32
	effect := func(ctx context.Context, tx *sql.Tx, r authority.Receipt, payload []byte) ([]byte, error) {
		effects.Add(1)
		return persistAdmission(ctx, tx, r, payload)
	}
	in := Inbox{DB: f.dbs["admission-controller"], Target: "admission-controller"}
	server := httptest.NewServer(in.Handler(func() (authority.Policy, authority.Identity, error) { return f.p, f.ids["admission-controller"], nil }, "admit-issue", "/admit", effect))
	defer server.Close()
	if err := f.dbs["intake"].WithTx(ctx, func(tx *sql.Tx) error {
		return Append(ctx, tx, f.p, f.ids["intake"], "ack-before-crash", "admit-issue", []byte(`{}`))
	}); err != nil {
		t.Fatal(err)
	}
	relay := Relay{DB: f.dbs["relay"], Identity: f.ids["relay"], Routes: map[string]string{"admit-issue": server.URL + "/admit"}}
	row, err := relay.reserve(ctx, f.p, time.Now())
	if err != nil || row == nil {
		t.Fatal("reservation failed")
	}
	var receipt authority.Receipt
	if err := authority.DecodeStrict(row.raw, &receipt); err != nil {
		t.Fatal(err)
	}
	if failure := relay.send(ctx, f.p, receipt, row.payload, time.Now()); failure != "" {
		t.Fatalf("receiver did not commit: %s", failure)
	}
	// Simulate process loss before finalize and expiry of its durable lease.
	if _, err := f.admin.Pool.ExecContext(ctx, `UPDATE authorized_outbox SET lease_until=now()-interval '1 second',next_attempt_at=now()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	restarted := relay
	if delivered, err := restarted.PollOnce(ctx, f.p, time.Now()); err != nil || !delivered {
		t.Fatalf("restart could not confirm committed receiver: %v", err)
	}
	if effects.Load() != 1 || count(t, f.admin, `SELECT count(*) FROM authorized_outbox WHERE attempts=2 AND published_at IS NOT NULL`) != 1 {
		t.Fatal("restart repeated receiver effect or lost attempts")
	}
}
