package service

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBuildBoundaryRequiresCompleteExplicitCapability(t *testing.T) {
	for _, mode := range []string{"maintenance", "shadow", "candidate", "verification", "unknown"} {
		for _, flag := range []string{"", "false", "true", "invalid"} {
			t.Run(mode+"-"+flag, func(t *testing.T) {
				t.Setenv("AI_SDLC_RUNTIME_MODE", mode)
				t.Setenv("AI_SDLC_INDEPENDENT_REVIEW", "true")
				t.Setenv("AI_SDLC_PUBLICATION", "true")
				t.Setenv("AI_SDLC_MERGE", "true")
				t.Setenv("AI_SDLC_BUILD", flag)
				mux := http.NewServeMux()
				mux.HandleFunc("POST /internal/build-merged", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
				for _, path := range []string{"/internal/build-merged", "/internal/build-merged?advance=1", "/internal/build", "/internal/build-merged/advance"} {
					w := httptest.NewRecorder()
					RuntimeBoundary(mux).ServeHTTP(w, httptest.NewRequest("POST", path, nil))
					if (w.Code == 204) != (mode == "verification" && flag == "true" && path == "/internal/build-merged") {
						t.Fatal("build route ceiling", path, w.Code)
					}
				}
				for _, option := range []string{"AI_SDLC_INDEPENDENT_REVIEW", "AI_SDLC_PUBLICATION", "AI_SDLC_MERGE"} {
					t.Setenv(option, "false")
					w := httptest.NewRecorder()
					RuntimeBoundary(mux).ServeHTTP(w, httptest.NewRequest("POST", "/internal/build-merged", nil))
					if w.Code == 204 {
						t.Fatal("build route survived missing prerequisite", option)
					}
					t.Setenv(option, "true")
				}
			})
		}
	}
}
