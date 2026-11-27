package mergegate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestLayeredNativeProtectionKeepsDedicatedFloor(t *testing.T) {
	for _, mode := range []string{"layered", "additional-check", "cross-app", "own-floor-missing", "own-bypass", "own-disabled", "wrong-name", "wrong-source", "wrong-target"} {
		t.Run(mode, func(t *testing.T) {
			p := PilotPolicy()
			p.Name = DedicatedName
			p.RulesetID = 999
			own := fixtureRules()
			other := fixtureRules()
			other[0]["parameters"].(map[string]any)["strict_required_status_checks_policy"] = false
			if mode == "own-floor-missing" {
				own[0]["parameters"].(map[string]any)["required_status_checks"] = []map[string]any{{"context": "Other", "integration_id": 15368}}
			}
			if mode == "additional-check" || mode == "cross-app" {
				name := "Extra required"
				app := int64(15368)
				if mode == "cross-app" {
					name = p.Required[0].Context
					app = 123
				}
				other[0]["parameters"].(map[string]any)["required_status_checks"] = []map[string]any{{"context": name, "integration_id": app}}
			}
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Error("unexpected native write")
				}
				if r.URL.Path == repoPath("/rulesets/999") {
					bypass := []any{}
					if mode == "own-bypass" {
						bypass = append(bypass, map[string]any{"actor_id": 5})
					}
					name, state, target := DedicatedName, "active", "refs/heads/main"
					if mode == "wrong-name" {
						name = "other"
					}
					if mode == "own-disabled" {
						state = "disabled"
					}
					if mode == "wrong-target" {
						target = "~ALL"
					}
					json.NewEncoder(w).Encode(map[string]any{"id": 999, "name": name, "target": "branch", "enforcement": state, "source": repository, "source_type": "Repository", "bypass_actors": bypass, "conditions": map[string]any{"ref_name": map[string]any{"include": []string{target}, "exclude": []string{}}}, "rules": own})
					return
				}
				effective := []map[string]any{}
				for index, rules := range [][]map[string]any{own, other} {
					id := 999
					if index == 1 {
						id = 9071701
					}
					for _, rule := range rules {
						copy := map[string]any{}
						for k, v := range rule {
							copy[k] = v
						}
						copy["ruleset_id"] = id
						copy["ruleset_source"] = repository
						copy["ruleset_source_type"] = "Repository"
						if mode == "wrong-source" && index == 1 {
							copy["ruleset_source"] = "other/repo"
						}
						effective = append(effective, copy)
					}
				}
				json.NewEncoder(w).Encode(effective)
			}))
			defer server.Close()
			c, _ := New(fixtureCredential())
			c.base = server.URL
			c.http.Transport.(*http.Transport).TLSClientConfig.RootCAs = server.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
			hash, checks, safe, e := c.nativePolicy(context.Background(), p)
			if mode == "layered" || mode == "additional-check" {
				if e != nil || !safe || hash == "" {
					t.Fatal("layered protection rejected", e, safe)
				}
				want := 3
				if mode == "additional-check" {
					want = 4
				}
				if len(checks) != want {
					t.Fatal("effective check lost", checks)
				}
			} else if e == nil && safe {
				t.Fatal("unsafe dedicated floor accepted")
			}
		})
	}
}

func TestInstalledPolicyRequiresPinnedIdentityAndExactCheckFloor(t *testing.T) {
	p := PilotPolicy()
	p.Name = DedicatedName
	p.RulesetID = 999
	file := filepath.Join(t.TempDir(), "policy.json")
	write := func(p Policy) {
		raw, _ := json.Marshal(p)
		if os.WriteFile(file, raw, 0600) != nil {
			t.Fatal("fixture")
		}
	}
	write(p)
	if got, e := LoadInstalled(file, ""); e != nil || got.RulesetID != 999 {
		t.Fatal("installed policy", e)
	}
	bad := p
	bad.Name = ""
	write(bad)
	if _, e := LoadInstalled(file, ""); e == nil {
		t.Fatal("unowned rule accepted")
	}
	bad = p
	bad.RulesetID = 0
	write(bad)
	if _, e := LoadInstalled(file, ""); e == nil {
		t.Fatal("missing pinned ID")
	}
	bad = p
	bad.Required = append([]RequiredCheck{}, p.Required...)
	bad.Required[0].AppID = 123
	write(bad)
	if _, e := LoadInstalled(file, ""); e == nil {
		t.Fatal("wrong check owner")
	}
	if os.WriteFile(file, []byte(`{"RulesetID":999,"RulesetID":1}`), 0600) != nil {
		t.Fatal("fixture")
	}
	if _, e := LoadInstalled(file, ""); e == nil {
		t.Fatal("duplicate config accepted")
	}
}
