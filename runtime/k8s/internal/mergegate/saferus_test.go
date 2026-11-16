package mergegate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestInstalledSaferusPolicyCannotCrossRepositoryOrWeakenChecks(t *testing.T) {
	p := PolicyForRepository("os-santiago/saferus")
	p.Name = DedicatedName
	p.RulesetID = 999
	path := filepath.Join(t.TempDir(), "policy.json")
	write := func(p Policy) {
		raw, _ := json.Marshal(p)
		if os.WriteFile(path, raw, 0600) != nil {
			t.Fatal("fixture")
		}
	}
	write(p)
	if _, err := LoadInstalledForRepository(path, "os-santiago/saferus"); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadInstalledForRepository(path, "os-santiago/homedir"); err != ErrGate {
		t.Fatal("cross-repo policy")
	}
	for _, mutate := range []func(*Policy){
		func(p *Policy) { p.Repository = "" },
		func(p *Policy) { p.Required[0].AppID = 99 },
		func(p *Policy) { p.Required[0].Context = "fake-quality" },
		func(p *Policy) { p.Required = nil },
	} {
		copy := p
		copy.Required = append([]RequiredCheck{}, p.Required...)
		mutate(&copy)
		write(copy)
		if _, err := LoadInstalledForRepository(path, "os-santiago/saferus"); err != ErrGate {
			t.Fatal("unsafe installed policy")
		}
	}
}
