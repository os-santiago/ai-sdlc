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
	} {
		m := generic()
		identity := c
		m.Publication.CommitIdentity = &identity
		if c.Validate() == nil || m.Validate() == nil {
			t.Fatalf("accepted invalid identity %q", c)
		}
	}
}
