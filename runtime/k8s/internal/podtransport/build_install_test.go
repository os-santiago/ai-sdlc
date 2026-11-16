package podtransport

import (
	"context"
	"net/http"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildworkload"
)

func TestBuildInstallationObserversNeverMutateOrAcceptMissingCustody(t *testing.T) {
	r, calls, _ := buildResourceFixture(t, func(_ *BuildResources, w http.ResponseWriter, request *http.Request) {
		if request.Method != "GET" {
			t.Error("installer observation performed a mutation")
		}
		w.WriteHeader(404)
	})
	ctx := context.Background()
	if _, e := ObserveBuildAdmission(ctx, r.client, []building.Slot{r.slot}, r.profile.PublisherImage); e == nil {
		t.Fatal("absent original custody accepted")
	}
	if calls.Load() != 1 {
		t.Fatal("scope failure did not stop further reads")
	}
	if _, e := ObserveBuildTrust(ctx, r.client, nil); e == nil {
		t.Fatal("unscoped trust observation accepted")
	}
	if _, e := ObserveBuildProfile(ctx, r.client, buildworkload.AdmissionConfiguration{}, []building.Slot{r.slot}, BuildTrust{}); e == nil {
		t.Fatal("missing separate approval accepted")
	}
	if calls.Load() != 1 {
		t.Fatal("invalid input reached API")
	}
}
