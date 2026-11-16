package execution

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/service"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/provenanceflow"
)

func TestProvenanceSignedInboxTraversesRuntimeBoundary(t *testing.T) {
	p := newProvenanceFixture(t)
	for k, v := range map[string]string{"AI_SDLC_RUNTIME_MODE": "verification", "AI_SDLC_INDEPENDENT_REVIEW": "true", "AI_SDLC_PUBLICATION": "true", "AI_SDLC_MERGE": "true", "AI_SDLC_BUILD": "true", "AI_SDLC_PROVENANCE": "true"} {
		t.Setenv(k, v)
	}
	var signed authority.Receipt
	if authority.DecodeStrict(p.receipt, &signed) != nil {
		t.Fatal("fixture receipt")
	}
	envelope := delivery.Envelope{Receipt: signed, Payload: p.payload}
	load := func() (authority.Policy, authority.Identity, error) {
		return p.cfg.Build.Outer, p.b.f.Material.Identities["verifier"], nil
	}
	inbox := delivery.Inbox{DB: p.j.DB, Target: "verifier"}
	mux := http.NewServeMux()
	mux.Handle("POST /internal/verify-built", inbox.Handler(load, "verify-built", "/internal/verify-built", provenanceflow.InboxEffect(p.j)))
	handler := service.RuntimeBoundary(mux)
	for _, valid := range []bool{false, true, true} {
		value := envelope
		if valid {
			carried, e := p.b.f.Material.Identities["relay"].Carry(p.cfg.Build.Outer, signed, http.MethodPost, "/internal/verify-built", time.Now())
			if e != nil {
				t.Fatal(e)
			}
			value.Transport = carried
		}
		raw, _ := json.Marshal(value)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("POST", "/internal/verify-built", bytes.NewReader(raw)))
		if (w.Code == 200) != valid {
			t.Fatal("signed HTTP boundary", valid, w.Code)
		}
	}
}
