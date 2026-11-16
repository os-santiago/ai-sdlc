package buildworkload

import (
	"bytes"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"testing"
)

func TestInstallationAdmissionDoesNotRequireSyntheticProfile(t *testing.T) {
	// Reuse the reviewed compiler fixture, then remove every operational proof.
	cfg, p, _, _ := setup(t)
	slot := cfg.Slots[0]
	a, b, e := Admission(p, []building.Slot{slot})
	if e != nil {
		t.Fatal(e)
	}
	input := p.admissionConfiguration()
	p.Dependencies = nil
	if p.Validate() == nil {
		t.Fatal("incomplete profile became operational")
	}
	x, y, e := AdmissionForInstallation(input, []building.Slot{slot})
	if e != nil || !bytes.Equal(a, x) || !bytes.Equal(b, y) {
		t.Fatal("installation changes reviewed policy", e)
	}
	input.Version = "ai-sdlc.saferus-build-profile/v1"
	if _, _, e = AdmissionForInstallation(input, []building.Slot{slot}); e == nil {
		t.Fatal("profile accepted as bootstrap input")
	}
}
