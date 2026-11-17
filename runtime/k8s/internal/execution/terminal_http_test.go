package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/completionflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/service"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/productionflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/terminalflow"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTerminalSignedHTTPBoundaries(t *testing.T) {
	for _, op := range []string{"record-deployment", "record-production"} {
		t.Run(op, func(t *testing.T) {
			for k, v := range map[string]string{"AI_SDLC_RUNTIME_MODE": "verification", "AI_SDLC_INDEPENDENT_REVIEW": "true", "AI_SDLC_PUBLICATION": "true", "AI_SDLC_MERGE": "true", "AI_SDLC_BUILD": "true", "AI_SDLC_PROVENANCE": "true", "AI_SDLC_RELEASE": "true"} {
				t.Setenv(k, v)
			}
			ctx := context.Background()
			var f deploymentFixture
			var db *store.DB
			var effect delivery.Effect
			role := "release-manager"
			if op == "record-deployment" {
				f, _ = completedDeployment(t)
				if sent, e := completionflow.DispatchDeployed(ctx, f.j.DB, completionLoader(f)); e != nil || !sent {
					t.Fatal(e)
				}
				db = f.p.j.DB
				j := store.NewTerminalJournal(db, func() (releasing.Configuration, error) { return *f.p.cfg, nil })
				effect = terminalflow.InboxEffect(j)
			} else {
				terminal := completedTerminal(t)
				f = terminal.f
				role = "orchestrator"
				db = f.p.b.f.Components[role].DB
				if sent, e := productionflow.DispatchCompleted(ctx, terminal.j.DB, productionLoader(terminal)); e != nil || !sent {
					t.Fatal(e)
				}
				effect = productionflow.InboxEffect(func() (authority.Policy, error) { return f.p.cfg.Provenance.Build.Outer, nil })
			}
			var raw, signed []byte
			if e := f.p.b.f.Admin.Pool.QueryRow(`SELECT payload,receipt FROM authorized_outbox WHERE operation=$1`, op).Scan(&raw, &signed); e != nil {
				t.Fatal(e)
			}
			var receipt authority.Receipt
			if authority.DecodeStrict(signed, &receipt) != nil {
				t.Fatal("signature")
			}
			path := "/internal/" + op
			policy := f.p.cfg.Provenance.Build.Outer
			key := f.p.b.f.Material.Identities[role]
			mux := http.NewServeMux()
			inbox := delivery.Inbox{DB: db, Target: role}
			mux.Handle("POST "+path, inbox.Handler(func() (authority.Policy, authority.Identity, error) { return policy, key, nil }, op, path, effect))
			server := httptest.NewServer(service.RuntimeBoundary(mux))
			defer server.Close()
			for _, signedTransport := range []bool{false, true} {
				envelope := delivery.Envelope{Receipt: receipt, Payload: raw}
				if signedTransport {
					envelope.Transport, _ = f.p.b.f.Material.Identities["relay"].Carry(policy, receipt, http.MethodPost, path, time.Now())
				}
				body, _ := json.Marshal(envelope)
				response, e := http.Post(server.URL+path, "application/json", bytes.NewReader(body))
				if e != nil {
					t.Fatal(e)
				}
				_ = response.Body.Close()
				if (response.StatusCode >= 200 && response.StatusCode < 300) != signedTransport {
					t.Fatal("signed boundary status", response.StatusCode)
				}
			}
			t.Setenv("AI_SDLC_RELEASE", "false")
			response, e := http.Post(server.URL+path, "application/json", bytes.NewReader([]byte(`{}`)))
			if e != nil {
				t.Fatal(e)
			}
			_ = response.Body.Close()
			if response.StatusCode != http.StatusServiceUnavailable {
				t.Fatal("disabled terminal route", response.StatusCode)
			}
		})
	}
}
