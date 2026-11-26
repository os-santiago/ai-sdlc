package project

import (
	"strings"
	"testing"
)

func TestCommitIdentityDefaultsAndRoundTrips(t *testing.T) {
	m := generic()
	if m.Publication.Identity() != DefaultCommitIdentity || DefaultCommitIdentity.Validate() != nil || strings.Contains(string(m.Canonical()), "commitIdentity") {
		t.Fatal("absent commitIdentity must use the default and stay out of the canonical form")
	}
	m.Publication.CommitIdentity = &CommitIdentity{Name: "Example Bot", Email: "bot+delivery@example.org"}
	parsed, err := ParseManifest(m.Canonical())
	if err != nil || parsed.Digest() != m.Digest() || parsed.Publication.Identity() != *m.Publication.CommitIdentity {
		t.Fatal("round trip", err)
	}
	if m.Digest() == generic().Digest() || !strings.Contains(string(m.Canonical()), `"commitIdentity":{"name":"Example Bot","email":"bot+delivery@example.org"}`) {
		t.Fatal("commitIdentity must be bound into the canonical form", string(m.Canonical()))
	}
	if _, err := ParseManifest([]byte(strings.Replace(string(m.Canonical()), `"name"`, `"extra":1,"name"`, 1))); err == nil {
		t.Fatal("unknown commitIdentity field accepted")
	}
}

func TestInvalidCommitIdentitiesAreRefused(t *testing.T) {
	for _, c := range []CommitIdentity{
		{},
		{Name: "Bot", Email: ""},
		{Name: "", Email: "bot@example.org"},
		{Name: "Bot <evil@example.org>", Email: "bot@example.org"},
		{Name: "Bot\nauthor x", Email: "bot@example.org"},
		{Name: "Bot‮", Email: "bot@example.org"},
		{Name: " Bot", Email: "bot@example.org"},
		{Name: "Bot.", Email: "bot@example.org"},
		{Name: "'Bot'", Email: "bot@example.org"},
		{Name: strings.Repeat("b", 101), Email: "bot@example.org"},
		{Name: "Bot", Email: "bot"},
		{Name: "Bot", Email: "bot@localhost"},
		{Name: "Bot", Email: "<bot@example.org>"},
		{Name: "Bot", Email: "bot @example.org"},
		{Name: "Bot", Email: "bot@example.org\n"},
		{Name: "\u00a0Bot", Email: "bot@example.org"},
		{Name: "Bot\u3000", Email: "bot@example.org"},
		{Name: "\u2003Bot", Email: "bot@example.org"},
		{Name: "Bot\u0085", Email: "bot@example.org"},
		{Name: "Bot\u2028", Email: "bot@example.org"},
		{Name: "Bot\u2029Two", Email: "bot@example.org"},
		{Name: "Bot\u2028Two", Email: "bot@example.org"},
	} {
		m := generic()
		identity := c
		m.Publication.CommitIdentity = &identity
		if c.Validate() == nil || m.Validate() == nil {
			t.Fatalf("accepted invalid identity %q", c)
		}
	}
	// Interior white space and non-ASCII letters remain valid.
	for _, c := range []CommitIdentity{{Name: "Équipe de livraison", Email: "bot@example.org"}, {Name: "ai-sdlc[bot]", Email: "noreply@ai-sdlc.invalid"}} {
		if c.Validate() != nil {
			t.Fatalf("refused valid identity %q", c)
		}
	}
}

// Bound resolves only the manifest a given policy binds; Lookup's fallback
// to a built-in or to another policy load's manifest never applies.
func TestBoundRefusesAManifestFromAnotherPolicyLoad(t *testing.T) {
	t.Cleanup(func() { SetInstalled(nil) })
	a, b := generic(), generic()
	b.Publication.CommitIdentity = &CommitIdentity{Name: "Other Bot", Email: "other@example.org"}
	SetInstalled(&a)
	if m, ok := Bound(a.Repository, a.Digest()); !ok || m.Digest() != a.Digest() {
		t.Fatal("bound manifest refused")
	}
	for name, c := range map[string]struct{ repository, digest string }{
		"other digest":          {a.Repository, b.Digest()},
		"no binding":            {a.Repository, ""},
		"other repository":      {"example/other", a.Digest()},
		"reference with digest": {Homedir, a.Digest()},
	} {
		if _, ok := Bound(c.repository, c.digest); ok {
			t.Fatal("bound:", name)
		}
	}
	SetInstalled(nil)
	if _, ok := Bound(a.Repository, a.Digest()); ok {
		t.Fatal("bound after the installed manifest was cleared")
	}
	if m, ok := Bound(Homedir, ""); !ok || m.Repository != Homedir {
		t.Fatal("reference repository without a binding")
	}
}
