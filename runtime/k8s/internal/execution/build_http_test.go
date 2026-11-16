package execution

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/service"
)

func TestBuildSignedInboxTraversesRuntimeBoundary(t *testing.T) {
	b := newBuildFixture(t)
	for k, v := range map[string]string{"AI_SDLC_RUNTIME_MODE": "verification", "AI_SDLC_INDEPENDENT_REVIEW": "true", "AI_SDLC_PUBLICATION": "true", "AI_SDLC_MERGE": "true", "AI_SDLC_BUILD": "true"} {
		t.Setenv(k, v)
	}
	var signed authority.Receipt
	if authority.DecodeStrict(b.receipt, &signed) != nil {
		t.Fatal("fixture receipt")
	}
	envelope := delivery.Envelope{Receipt: signed, Payload: b.raw}
	load := func() (authority.Policy, authority.Identity, error) {
		return b.cfg.Outer, b.f.Material.Identities["build-controller"], nil
	}
	inbox := delivery.Inbox{DB: b.journal.DB, Target: "build-controller"}
	mux := http.NewServeMux()
	mux.Handle("POST /internal/build-merged", inbox.Handler(load, "build-merged", "/internal/build-merged", buildflow.InboxEffect(b.journal)))
	handler := service.RuntimeBoundary(mux)
	for _, valid := range []bool{false, true, true} {
		value := envelope
		if valid {
			carried, e := b.f.Material.Identities["relay"].Carry(b.cfg.Outer, signed, http.MethodPost, "/internal/build-merged", time.Now())
			if e != nil {
				t.Fatal(e)
			}
			value.Transport = carried
		}
		raw, _ := json.Marshal(value)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("POST", "/internal/build-merged", bytes.NewReader(raw)))
		if (w.Code == 200) != valid {
			t.Fatal("signed HTTP boundary", valid, w.Code)
		}
	}
}
