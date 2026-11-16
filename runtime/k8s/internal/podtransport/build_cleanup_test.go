package podtransport

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

func TestBuildCleanupUsesOnlyOriginalUIDAndFreshCustody(t *testing.T) {
	for _, scenario := range []string{"absent", "original", "replaced", "delete-lost", "delete-forbidden", "stale-node", "stale-lease", "foreign-pvc-consumer", "remaining-task", "remaining-root", "unknown-create", "empty", "expired-authority"} {
		t.Run(scenario, func(t *testing.T) {
			deletes := 0
			r, _, _ := buildResourceFixture(t, func(r *BuildResources, w http.ResponseWriter, req *http.Request) {
				if req.Method == "DELETE" {
					deletes++
					if req.URL.Path != runPath(r.runName("build")) {
						t.Error("unexpected cleanup target")
					}
					var body map[string]any
					if json.NewDecoder(req.Body).Decode(&body) != nil || body["propagationPolicy"] != "Foreground" || len(body) != 4 || body["preconditions"].(map[string]any)["uid"] != fixtureConfigUID {
						t.Error("missing finite UID CAS")
					}
					if scenario == "delete-forbidden" {
						w.WriteHeader(403)
						return
					}
					if scenario == "replaced" {
						w.WriteHeader(409)
						return
					}
					if scenario == "delete-lost" {
						w.WriteHeader(500)
						return
					}
					w.WriteHeader(202)
					return
				}
				if req.Method != "GET" {
					t.Error("execution effect during cleanup")
				}
				if obj := syntheticSlotResponse(r, req.URL.Path); obj != nil {
					if scenario == "stale-node" && strings.Contains(req.URL.Path, "/nodes/") {
						obj["metadata"].(map[string]any)["uid"] = fixtureOtherUID
					}
					if scenario == "stale-lease" && strings.Contains(req.URL.Path, "/leases/") {
						obj["spec"].(map[string]any)["renewTime"] = time.Now().Add(-time.Minute).Format(time.RFC3339Nano)
					}
					_ = json.NewEncoder(w).Encode(obj)
					return
				}
				if strings.HasSuffix(req.URL.Path, "/taskruns") || strings.HasSuffix(req.URL.Path, "/pods") {
					version, kind, _, _ := buildListType(req.URL.Path)
					items := []any{}
					if scenario == "foreign-pvc-consumer" && strings.HasSuffix(req.URL.Path, "/pods") {
						items = append(items, map[string]any{"metadata": map[string]any{"uid": fixtureOtherUID}, "spec": map[string]any{"volumes": []any{map[string]any{"persistentVolumeClaim": map[string]any{"claimName": r.slot.PVC}}}}})
					}
					if scenario == "remaining-task" && strings.HasSuffix(req.URL.Path, "/taskruns") {
						items = append(items, map[string]any{"metadata": map[string]any{"ownerReferences": []any{map[string]any{"kind": "PipelineRun", "uid": fixtureConfigUID}}}})
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": version, "kind": kind, "metadata": map[string]any{}, "items": items})
					return
				}
				if req.URL.Path == runPath(r.runName("build")) {
					if scenario == "remaining-root" {
						_ = json.NewEncoder(w).Encode(runResponse(r, "build"))
						return
					}
					w.WriteHeader(404)
					return
				}
				t.Error("unexpected cleanup API path", req.URL.Path)
				w.WriteHeader(500)
			})
			claim := store.BuildClaim{ID: r.command.ID(), Token: fixtureConfigUID, SlotID: r.slot.ID(), SlotDigest: r.slot.Digest()}
			original := store.BuildCleanup{Claim: claim, Slot: r.slot, Resources: []store.CleanupResource{{Kind: "build", Name: r.runName("build"), UID: fixtureConfigUID}}}
			if scenario == "unknown-create" {
				original.UnknownCreate = true
			}
			if scenario == "empty" {
				original.Resources = nil
			}
			if scenario == "expired-authority" {
				r.compiled.ExpiresAt = time.Now().Add(-time.Hour)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			proof, err := cleanupBuildResources(ctx, r.client, original)
			good := scenario == "absent" || scenario == "original" || scenario == "empty" || scenario == "expired-authority"
			if good != (err == nil) {
				t.Fatalf("good=%v err=%v", good, err)
			}
			if good && (proof.ObservedAbsentAt == 0 || proof.BuildID != claim.ID || proof.UnknownCreate) {
				t.Fatal("invalid cleanup proof")
			}
			if (scenario == "empty") && deletes != 0 {
				t.Fatal("delete without original fresh custody")
			}
			if (scenario == "stale-node" || scenario == "stale-lease") && deletes != 1 {
				t.Fatal("known original root was not contained before custody proof failed")
			}
			if deletes > 1 {
				t.Fatal("delete retry exceeded one call per reconciliation")
			}
		})
	}
}
