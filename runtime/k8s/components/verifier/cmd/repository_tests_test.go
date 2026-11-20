package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/repositorytests"
)

func TestRepositoryTestsEntrypointWritesOneResultLine(t *testing.T) {
	dir := t.TempDir()
	workspace, scratch := filepath.Join(dir, "ws"), filepath.Join(dir, "tmp")
	for _, d := range []string{workspace, scratch} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	v := project.Verification{Image: "registry.example/go@sha256:" + strings.Repeat("a", 64), Command: []string{"/bin/sh", "-c", "exit 7"},
		TimeoutSeconds: 30, CPU: "1", Memory: "1Gi", EphemeralStorage: "1Gi", Network: "none", Result: project.Result{ExitCode: true},
		Repeat: 1, TestGlobs: []string{"**/*_test.go"}}
	raw, _ := json.Marshal(v)
	settings := filepath.Join(dir, "verification.json")
	if err := os.WriteFile(settings, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runRepositoryTests([]string{settings, workspace, scratch}, &out); err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSuffix(out.String(), "\n")
	if strings.Contains(line, "\n") {
		t.Fatal("more than one line", out.String())
	}
	if run, err := repositorytests.ParseRun([]byte(line)); err != nil || run.ExitCode != 7 {
		t.Fatal("result", run, err)
	}
	v.Network = "default"
	raw, _ = json.Marshal(v)
	if err := os.WriteFile(settings, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runRepositoryTests([]string{settings, workspace, scratch}, &bytes.Buffer{}); err == nil {
		t.Fatal("settings outside the reviewed rules accepted")
	}
	if err := runRepositoryTests([]string{settings}, &bytes.Buffer{}); err == nil {
		t.Fatal("wrong arguments accepted")
	}
}
