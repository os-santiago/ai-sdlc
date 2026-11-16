package execution

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildworkload"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
)

func TestBuildFailureStillAttemptsCleanupAfterConcurrentExpiry(t *testing.T) {
	b := newBuildFixture(t)
	slot := b.cfg.Slots[0]
	profile := buildworkload.Profile{Version: "ai-sdlc.saferus-build-profile/v1", ClusterUID: slot.ClusterUID, NamespaceUID: slot.NamespaceUID, PublisherImage: "quay.io/os-santiago/ai-sdlc@sha256:" + strings.Repeat("a", 64), Dependencies: map[string]buildworkload.Dependency{}, DeniedTargets: []buildworkload.DeniedTarget{{Host: "10.96.0.1", Port: 443}, {Host: "10.96.0.2", Port: 443}}}
	for _, k := range buildworkload.DependencyKeys() {
		profile.Dependencies[k] = buildworkload.Dependency{UID: slot.NodeUID, SpecSHA256: strings.Repeat("a", 64)}
	}
	b.cfg.ProfileSHA256 = profile.Digest()
	b.command.ProfileSHA256 = b.cfg.ProfileDigest()
	b.command.ExpiresAt = time.Now().Add(19 * time.Minute).UnixMilli()
	b.raw, _ = json.Marshal(b.command)
	signed, e := b.f.Material.Identities["release-manager"].Seal(b.cfg.Outer, b.command.ID(), "build-merged", b.raw)
	if e != nil {
		t.Fatal(e)
	}
	b.receipt, _ = json.Marshal(signed)
	ctx := context.Background()
	if e = b.journal.QueueBuild(ctx, b.raw, b.receipt); e != nil {
		t.Fatal(e)
	}
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Error("fixture must never create/delete a workload")
		}
		if calls.Add(1) == 1 {
			result, e := b.f.Admin.Pool.ExecContext(ctx, `UPDATE build_requests SET state='failed',reason='expired' WHERE id=$1 AND state='claimed'`, b.command.ID())
			if e != nil {
				t.Error(e)
			} else if n, e := result.RowsAffected(); e != nil || n != 1 {
				t.Error("claim not terminalized")
			}
		}
		w.WriteHeader(500)
	}))
	defer server.Close()
	client, e := podtransport.NewClient(podtransport.Config{APIServer: server.URL, CA: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), BearerToken: strings.Repeat("x", 32)})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = podtransport.ExecuteQueuedBuild(ctx, client, b.journal, b.command.ID(), *b.cfg, profile); e == nil {
		t.Fatal("preflight failure accepted")
	}
	if calls.Load() < 2 {
		t.Fatal("terminal cleanup skipped after StopBuild returned zero rows")
	}
	var cleanups int
	if e = b.f.Admin.Pool.QueryRowContext(ctx, `SELECT count(*) FROM build_cleanups WHERE request_id=$1`, b.command.ID()).Scan(&cleanups); e != nil || cleanups != 0 {
		t.Fatal("failed cleanup acknowledged", e)
	}
}
