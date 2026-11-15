package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixtureCredential() Credential {
	return Credential{Version: 1, Mode: "gh-cli", Host: "github.com", Repository: "example/project", ActorID: 42, ActorLogin: "fixture", Token: "fixture_credential_value_not_a_real_token"}
}
func serverClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	c, err := New(fixtureCredential(), "example/project")
	if err != nil {
		t.Fatal(err)
	}
	c.base = server.URL
	return c
}
func issueJSON() string {
	return `{"id":7,"number":3,"title":"fixture issue","body":"test","state":"open","user":{"id":42,"login":"fixture"},"labels":[{"name":"ready"}],"updated_at":"2026-01-01T00:00:00Z"}`
}

func TestCredentialScopeAndBoundedFile(t *testing.T) {
	for _, mutate := range []func(*Credential){func(c *Credential) { c.Mode = "app" }, func(c *Credential) { c.Repository = "other/project" }, func(c *Credential) { c.Host = "other.invalid" }, func(c *Credential) { c.Token += "\nheader" }, func(c *Credential) { c.ActorID = 0 }} {
		c := fixtureCredential()
		mutate(&c)
		if _, err := New(c, "example/project"); err == nil {
			t.Fatal("invalid credential accepted")
		}
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "credential.json")
	raw, _ := json.Marshal(fixtureCredential())
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, "example/project"); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{strings.Repeat(" ", 16385), `{"version":1,"version":1}`} {
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path, "example/project"); err == nil {
			t.Fatal("invalid file accepted")
		}
	}
}

func TestFixedRepositoryReadsAndIssueIdentity(t *testing.T) {
	c := serverClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || !strings.HasPrefix(r.URL.Path, "/repos/example/project/") || r.Header.Get("Authorization") != "Bearer "+fixtureCredential().Token {
			t.Error("request scope/auth mismatch")
		}
		switch r.URL.Path {
		case "/repos/example/project/issues":
			if r.URL.Query().Get("sort") != "created" || r.URL.Query().Get("per_page") != "100" || r.URL.Query().Get("state") != "all" || r.URL.Query().Get("since") != "2026-01-01T00:00:00Z" {
				t.Error("pagination/since missing")
			}
			fmt.Fprint(w, "["+issueJSON()+"]")
		case "/repos/example/project/issues/3":
			fmt.Fprint(w, issueJSON())
		case "/repos/example/project/commits/main":
			fmt.Fprintf(w, `{"sha":%q}`, strings.Repeat("a", 40))
		default:
			fmt.Fprint(w, issueJSON())
		}
	})
	issues, err := c.Issues(context.Background(), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), 1)
	if err != nil || len(issues) != 1 || !issues[0].HasLabel("ready") {
		t.Fatalf("issues: %v", err)
	}
	if _, err := c.Issue(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Issue(context.Background(), 4); err == nil {
		t.Fatal("wrong issue identity accepted")
	}
	if sha, err := c.BaseSHA(context.Background(), "main"); err != nil || sha != strings.Repeat("a", 40) {
		t.Fatalf("base: %v", err)
	}
}

func TestNullListsAreNotSuccessfulEmptyScans(t *testing.T) {
	c := serverClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "null") })
	if _, err := c.Issues(context.Background(), time.Time{}, 1); err == nil {
		t.Fatal("null issues treated as complete scan")
	}
	if _, _, err := c.LabelActor(context.Background(), 3, "ready"); err == nil {
		t.Fatal("null events treated as authoritative absence")
	}
}

func TestLabelAuthorizationUsesLatestEventAcrossPages(t *testing.T) {
	removed := false
	c := serverClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "1" {
			events := make([]LabelEvent, 100)
			for i := range events {
				events[i] = LabelEvent{ID: int64(i + 1), Event: "labeled", Actor: Actor{ID: 42, Login: "fixture"}, Label: Label{Name: "ready"}}
			}
			json.NewEncoder(w).Encode(events)
		} else {
			event := "labeled"
			if removed {
				event = "unlabeled"
			}
			json.NewEncoder(w).Encode([]LabelEvent{{ID: 102, Event: event, Actor: Actor{ID: 77, Login: "later"}, Label: Label{Name: "ready"}}, {ID: 101, Event: "labeled", Actor: Actor{ID: 42, Login: "fixture"}, Label: Label{Name: "ready"}}})
		}
	})
	actor, id, err := c.LabelActor(context.Background(), 3, "ready")
	if err != nil || actor.ID != 77 || id != 102 {
		t.Fatalf("latest authorization: %v %d %v", actor, id, err)
	}
	removed = true
	actor, id, err = c.LabelActor(context.Background(), 3, "ready")
	if err != nil || actor.ID != 0 || id != 0 {
		t.Fatal("removed label retained authority")
	}
}

func TestRedirectRateLimitOversizeAndMalformedFailClosed(t *testing.T) {
	var destinationCalls int
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { destinationCalls++ }))
	defer destination.Close()
	for _, mode := range []string{"redirect", "rate", "oversize", "malformed", "pr"} {
		t.Run(mode, func(t *testing.T) {
			c := serverClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "redirect":
					http.Redirect(w, r, destination.URL, 302)
				case "rate":
					w.Header().Set("Retry-After", "90")
					w.WriteHeader(429)
					fmt.Fprint(w, fixtureCredential().Token)
				case "oversize":
					fmt.Fprint(w, strings.Repeat("x", 2*1024*1024+1))
				case "malformed":
					fmt.Fprint(w, `{"broken":`)
				case "pr":
					fmt.Fprint(w, strings.TrimSuffix(issueJSON(), "}")+`,"pull_request":{"url":"ignored"}}`)
				}
			})
			_, err := c.Issue(context.Background(), 3)
			if err == nil || strings.Contains(err.Error(), fixtureCredential().Token) {
				t.Fatal("unsafe API failure")
			}
			if mode == "rate" {
				var api *APIError
				if !errors.As(err, &api) || api.RetryAfter != 90*time.Second {
					t.Fatal("rate delay not retained")
				}
			}
		})
	}
	if destinationCalls != 0 {
		t.Fatal("credential forwarded on redirect")
	}
}
