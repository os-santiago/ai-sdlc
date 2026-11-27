package verification

import (
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
)

// The manifest refuses services that cannot fit the command lifetime; its
// copy of that lifetime must stay MaxCommandAge.
func TestManifestServiceBudgetIsTheCommandLifetime(t *testing.T) {
	if time.Duration(project.MaxVerificationSeconds)*time.Second != MaxCommandAge {
		t.Fatal("project.MaxVerificationSeconds is out of date")
	}
}
