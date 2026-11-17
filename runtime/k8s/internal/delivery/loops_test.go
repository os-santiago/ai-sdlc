package delivery

import (
	"context"
	"database/sql"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func loopPolicies(t *testing.T, f fixture) (authority.Policy, authority.Policy) {
	t.Helper()
	inner := f.p
	inner.Repository = "os-santiago/saferus"
	inner.Environment = "ai-sdlc-production"
	inner.Autonomy = "auto-merge-low"
	inner.Grants = nil
	inner.Keys = map[string]authority.PublicKey{}
	outer := inner
	outer.Autonomy = "auto-deploy"
	outer.Keys = map[string]authority.PublicKey{}
	for id, k := range f.p.Keys {
		if k.Role != "build-controller" {
			inner.Keys[id] = k
		}
		switch k.Role {
		case "release-manager", "build-controller", "verifier", "relay", "deployer", "orchestrator":
			outer.Keys[id] = k
		}
	}
	for _, g := range f.p.Grants {
		if authority.OuterOperation(g.Operation) {
			outer.Grants = append(outer.Grants, g)
		} else if g.Operation != "record-release" && g.Operation != "record-attempt" {
			inner.Grants = append(inner.Grants, g)
		}
	}
	if e := authority.ValidateLoopPolicies(inner, outer); e != nil {
		t.Fatal(e)
	}
	return inner, outer
}
func TestInterleavedLoopRelayUsesOneAttemptAndOwnPolicy(t *testing.T) {
	f := setup(t)
	inner, outer := loopPolicies(t, f)
	ctx := context.Background()
	var calls atomic.Int32
	mux := http.NewServeMux()
	for _, op := range []string{"admit-issue", "build-merged", "verify-built"} {
		caller, target, e := authority.OperationOwner(op)
		if e != nil {
			t.Fatal(e)
		}
		p := inner
		if authority.OuterOperation(op) {
			p = outer
		}
		in := Inbox{DB: f.dbs[target], Target: target}
		handler := in.Handler(func() (authority.Policy, authority.Identity, error) { return p, f.ids[target], nil }, op, "/"+op,
			func(context.Context, *sql.Tx, authority.Receipt, []byte) ([]byte, error) {
				calls.Add(1)
				return []byte(`{"accepted":true}`), nil
			})
		mux.Handle("/"+op, handler)
		if e := f.dbs[caller].WithTx(ctx, func(tx *sql.Tx) error { return Append(ctx, tx, p, f.ids[caller], "loop-"+op, op, []byte(`{}`)) }); e != nil {
			t.Fatal(e)
		}
	}
	server := httptest.NewServer(mux)
	defer server.Close()
	relay := Relay{DB: f.dbs["relay"], Identity: f.ids["relay"], Routes: map[string]string{}}
	for _, op := range []string{"admit-issue", "build-merged", "verify-built"} {
		relay.Routes[op] = server.URL + "/" + op
	}
	frozen := outer
	frozen.Freeze = true
	if _, e := relay.PollLoops(ctx, inner, frozen, time.Now()); e == nil {
		t.Fatal("outer freeze ignored")
	}
	if count(t, f.admin, `SELECT count(*) FROM authorized_outbox WHERE attempts<>0`) != 0 {
		t.Fatal("freeze consumed attempt")
	}
	for range 3 {
		if ok, e := relay.PollLoops(ctx, inner, outer, time.Now()); e != nil || !ok {
			t.Fatalf("delivery %t: %v", ok, e)
		}
	}
	if calls.Load() != 3 || count(t, f.admin, `SELECT count(*) FROM authorized_outbox WHERE attempts=1 AND published_at IS NOT NULL`) != 3 {
		t.Fatal("wrong policy consumed attempt or repeated effect")
	}
	// Builder identity has transport/inbox access only. Installing its identity
	// does not confer generation, publication, merge or build journal privileges.
	for _, q := range []string{`SELECT * FROM execution_requests`, `SELECT * FROM published_records`, `SELECT * FROM merged_records`, `SELECT * FROM release_gates`} {
		if _, e := f.dbs["build-controller"].Pool.ExecContext(ctx, q); e == nil {
			t.Fatal("builder inherited unrelated privilege")
		}
	}
}
