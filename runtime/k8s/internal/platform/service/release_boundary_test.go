package service

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReleaseBoundaryRequiresCompleteExplicitCapability(t *testing.T) {
	for _, mode := range []string{"maintenance", "shadow", "candidate", "verification", "unknown"} {
		for _, flag := range []string{"", "false", "true", "invalid"} {
			t.Run(mode+"-"+flag, func(t *testing.T) {
				values := map[string]string{"AI_SDLC_RUNTIME_MODE": mode, "AI_SDLC_INDEPENDENT_REVIEW": "true", "AI_SDLC_PUBLICATION": "true", "AI_SDLC_MERGE": "true", "AI_SDLC_BUILD": "true", "AI_SDLC_PROVENANCE": "true", "AI_SDLC_RELEASE": flag}
				for k, v := range values {
					t.Setenv(k, v)
				}
				handler := RuntimeBoundary(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
				for _, operation := range []string{"promote-artifact", "deploy-release"} {
					for _, method := range []string{"POST", "GET", "PUT"} {
						for _, suffix := range []string{"", "?advance=1", "/advance"} {
							w := httptest.NewRecorder()
							handler.ServeHTTP(w, httptest.NewRequest(method, "/internal/"+operation+suffix, nil))
							expected := mode == "verification" && flag == "true" && method == "POST" && suffix == ""
							if (w.Code == 204) != expected {
								t.Fatal("release route ceiling", method, operation, suffix, w.Code)
							}
						}
					}
				}
				for key := range values {
					if key == "AI_SDLC_RUNTIME_MODE" || key == "AI_SDLC_RELEASE" {
						continue
					}
					t.Setenv(key, "false")
					for _, operation := range []string{"promote-artifact", "deploy-release"} {
						w := httptest.NewRecorder()
						handler.ServeHTTP(w, httptest.NewRequest("POST", "/internal/"+operation, nil))
						if w.Code == 204 {
							t.Fatal("missing prerequisite accepted", key)
						}
					}
					t.Setenv(key, "true")
				}
			})
		}
	}
}
