package service

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProvenanceBoundaryRequiresCompleteExplicitCapability(t *testing.T) {
	for _, mode := range []string{"maintenance", "shadow", "candidate", "verification", "unknown"} {
		for _, flag := range []string{"", "false", "true", "invalid"} {
			t.Run(mode+"-"+flag, func(t *testing.T) {
				t.Setenv("AI_SDLC_RUNTIME_MODE", mode)
				t.Setenv("AI_SDLC_INDEPENDENT_REVIEW", "true")
				t.Setenv("AI_SDLC_PUBLICATION", "true")
				t.Setenv("AI_SDLC_MERGE", "true")
				t.Setenv("AI_SDLC_BUILD", "true")
				t.Setenv("AI_SDLC_PROVENANCE", flag)
				mux := http.NewServeMux()
				mux.HandleFunc("POST /internal/verify-built", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
				for _, path := range []string{"/internal/verify-built", "/internal/verify-built?advance=1", "/internal/build", "/internal/verify-built/advance"} {
					w := httptest.NewRecorder()
					RuntimeBoundary(mux).ServeHTTP(w, httptest.NewRequest("POST", path, nil))
					if (w.Code == 204) != (mode == "verification" && flag == "true" && path == "/internal/verify-built") {
						t.Fatal("build route ceiling", path, w.Code)
					}
				}
				for _, option := range []string{"AI_SDLC_INDEPENDENT_REVIEW", "AI_SDLC_PUBLICATION", "AI_SDLC_MERGE", "AI_SDLC_BUILD"} {
					t.Setenv(option, "false")
					w := httptest.NewRecorder()
					RuntimeBoundary(mux).ServeHTTP(w, httptest.NewRequest("POST", "/internal/verify-built", nil))
					if w.Code == 204 {
						t.Fatal("build route survived missing prerequisite", option)
					}
					t.Setenv(option, "true")
				}
			})
		}
	}
}
