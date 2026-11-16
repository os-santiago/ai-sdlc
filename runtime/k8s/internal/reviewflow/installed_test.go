package reviewflow

import (
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstalledReviewConfigRejectsEndpointAndGenerationPurpose(t *testing.T) {
	dir := t.TempDir()
	configPath, tokenPath := filepath.Join(dir, "config.json"), filepath.Join(dir, "token")
	if e := os.WriteFile(tokenPath, []byte(strings.Repeat("x", 48)), 0600); e != nil {
		t.Fatal(e)
	}
	adev := "bounded independent review"
	c := InstalledConfiguration{Version: "ai-sdlc.independent-review-runtime/v1", ADev: adev, WorkerImage: "quay.io/os-santiago/ai-sdlc@sha256:" + strings.Repeat("a", 64), MailboxImage: "quay.io/os-santiago/ai-sdlc@sha256:" + strings.Repeat("b", 64), Profile: executor.Profile{Version: executor.ReviewProfileVersion, Model: "codex-cli", MaxSteps: 8, MaxSeconds: 180, MaxTokens: 4096, ADevSHA256: authority.Digest([]byte(adev))}}
	write := func() {
		b, _ := json.Marshal(c)
		if e := os.WriteFile(configPath, b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	write()
	got, e := LoadInstalled(configPath, tokenPath)
	if e != nil || got.Namespace != "ai-sdlc-reviewers" || got.Model.Endpoint != "http://127.0.0.1:8787/v1/chat/completions" {
		t.Fatal("valid independent profile rejected", e)
	}
	c.Profile.Version = executor.ProfileVersion
	write()
	if _, e = LoadInstalled(configPath, tokenPath); e == nil {
		t.Fatal("generation profile accepted as review")
	}
	c.Profile.Version = executor.ReviewProfileVersion
	c.WorkerImage = "mutable:latest"
	write()
	if _, e = LoadInstalled(configPath, tokenPath); e == nil {
		t.Fatal("mutable image accepted")
	}
	c.WorkerImage = got.WorkerImage
	b, _ := json.Marshal(c)
	var bad map[string]any
	json.Unmarshal(b, &bad)
	bad["endpoint"] = "https://outside.invalid/v1/chat/completions"
	b, _ = json.Marshal(bad)
	os.WriteFile(configPath, b, 0600)
	if _, e = LoadInstalled(configPath, tokenPath); e == nil {
		t.Fatal("endpoint override accepted")
	}
}
