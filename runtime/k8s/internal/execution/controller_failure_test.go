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

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelbroker"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestControllerStopsTransferredClaimOnResourceFailures(t *testing.T) {
	for _, scenario := range []string{"lost-config-ack", "pod-failure", "cleanup-denied"} {
		t.Run(scenario, func(t *testing.T) {
			f := newLifecycleFixture(t)
			const uid = "11111111-1111-4111-8111-111111111111"
			var config core.ConfigMap
			var posts, deletes atomic.Int32
			removed := false
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == "POST":
					posts.Add(1)
					if strings.HasSuffix(r.URL.Path, "/pods") {
						w.WriteHeader(503)
						return
					}
					if !strings.HasSuffix(r.URL.Path, "/configmaps") || json.NewDecoder(r.Body).Decode(&config) != nil {
						t.Error("unexpected create")
						w.WriteHeader(400)
						return
					}
					config.UID = types.UID(uid)
					if scenario == "lost-config-ack" {
						connection, _, _ := w.(http.Hijacker).Hijack()
						connection.Close()
						return
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(201)
					json.NewEncoder(w).Encode(config)
				case r.Method == "GET":
					if !strings.HasSuffix(r.URL.Path, "/configmaps/"+config.Name) {
						t.Error("unexpected observation")
						w.WriteHeader(400)
						return
					}
					if removed {
						w.WriteHeader(404)
						return
					}
					json.NewEncoder(w).Encode(config)
				case r.Method == "DELETE":
					deletes.Add(1)
					var options meta.DeleteOptions
					if !strings.HasSuffix(r.URL.Path, "/configmaps/"+config.Name) || json.NewDecoder(r.Body).Decode(&options) != nil || options.Preconditions == nil || options.Preconditions.UID == nil || string(*options.Preconditions.UID) != uid {
						t.Error("delete without exact UID")
						w.WriteHeader(400)
						return
					}
					if scenario == "cleanup-denied" {
						w.WriteHeader(403)
						return
					}
					removed = true
					w.WriteHeader(200)
				default:
					t.Error("unexpected mutation")
					w.WriteHeader(400)
				}
			}))
			defer server.Close()
			client, err := podtransport.NewClient(podtransport.Config{APIServer: server.URL, CA: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), BearerToken: strings.Repeat("x", 32)})
			if err != nil {
				t.Fatal(err)
			}
			provider, err := modelbroker.NewProvider(modelbroker.ProviderConfig{Endpoint: "http://127.0.0.1:1/v1/chat/completions", BearerToken: strings.Repeat("x", 32), AllowLoopbackHTTP: true})
			if err != nil {
				t.Fatal(err)
			}
			record, err := RunController(context.Background(), f.db, f.loadLifecycle, f.prepared, client, provider, func(context.Context) (ModelPolicy, error) {
				t.Error("unexpected model activation")
				return ModelPolicy{}, ErrAuthority
			})
			failure, ok := err.(ControllerError)
			if !ok || record.RequestID != "" {
				t.Fatal("failed controller returned candidate", err)
			}
			stage := "pod"
			expectedPosts := int32(2)
			expectedDeletes := int32(1)
			if scenario == "lost-config-ack" {
				stage = "config"
				expectedPosts = 1
				expectedDeletes = 0
			}
			if scenario == "cleanup-denied" {
				stage = "cleanup"
			}
			if failure.Stage != stage || posts.Load() != expectedPosts || deletes.Load() != expectedDeletes {
				t.Fatal("unexpected retry or cleanup", failure, posts.Load(), deletes.Load())
			}
			var token, state string
			if f.Admin.Pool.QueryRow(`SELECT claim_token,state FROM execution_requests WHERE id=$1`, f.claim.ID).Scan(&token, &state) != nil || token == f.claim.Token || state != "stopped" {
				t.Fatal("transferred owner remained live")
			}
			if f.Count(t, `SELECT count(*) FROM execution_effects WHERE result IS NULL`) != 1 {
				t.Fatal("uncertain evidence removed")
			}
			if scenario != "lost-config-ack" && f.Count(t, `SELECT count(*) FROM execution_effects WHERE result IS NOT NULL`) != 1 {
				t.Fatal("acknowledged evidence removed")
			}
		})
	}
}
