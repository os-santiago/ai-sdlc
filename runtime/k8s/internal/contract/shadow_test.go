package contract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShadowContractRequiresExplicitPolicy(t *testing.T) {
	valid := "spec: '1.0.0'\nautonomy:\n  level: shadow\ntriggers:\n  issue_label: ready\n  min_body_length: 120\n  acceptance_criteria: {min: 1, max: 8}\n"
	path := filepath.Join(t.TempDir(), "contract.yaml")
	for _, raw := range []string{"", valid + "unexpected: true\n", valid + "---\n{}\n", strings.Replace(valid, "level: shadow", "level: auto-deploy", 1), strings.Replace(valid, "max: 8", "max: 100", 1), strings.Replace(valid, "120", "0", 1)} {
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadShadow(path, "ready"); err == nil {
			t.Fatal("invalid policy accepted")
		}
	}
	if err := os.WriteFile(path, []byte(valid), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadShadow(path, "ready"); err != nil {
		t.Fatal(err)
	}
}
