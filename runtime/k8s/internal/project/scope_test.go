package project

import "testing"

func TestInstalledManifestExtendsOnlyItsRepository(t *testing.T) {
	t.Cleanup(func() { SetInstalled(nil) })
	if Supported("example/service") {
		t.Fatal("unbound repository supported")
	}
	m := generic()
	SetInstalled(&m)
	m.Repository = "example/mutated" // the registry keeps its own copy
	if !Supported("example/service") || Supported("example/other") || !Supported(Saferus) || !Supported(Homedir) {
		t.Fatal("support decision")
	}
	if got, ok := Lookup("example/service"); !ok || got.Repository != "example/service" {
		t.Fatal("installed lookup")
	}
	if got, ok := Lookup(Saferus); !ok || len(got.Recipes) != 1 {
		t.Fatal("reference lookup")
	}
	SetInstalled(nil)
	if Supported("example/service") {
		t.Fatal("cleared manifest still supported")
	}
}

// BoundOrNone prepares a repository with no manifest at all as before, and
// otherwise refuses every manifest the policy does not bind.
func TestBoundOrNoneAllowsOnlyAnAbsentManifest(t *testing.T) {
	t.Cleanup(func() { SetInstalled(nil) })
	m := generic()
	digest := m.Digest()
	if got, ok := BoundOrNone("example/service", ""); !ok || got.Repository != "" {
		t.Fatal("an unbound repository without any manifest refused")
	}
	if got, ok := BoundOrNone(Saferus, ""); !ok || got.Repository != Saferus {
		t.Fatal("reference refused")
	}
	if _, ok := BoundOrNone("example/service", digest); ok {
		t.Fatal("a cleared registry satisfied a bound policy")
	}
	SetInstalled(&m)
	if got, ok := BoundOrNone("example/service", digest); !ok || got.Digest() != digest {
		t.Fatal("the bound manifest refused")
	}
	if _, ok := BoundOrNone("example/service", ""); ok {
		t.Fatal("an unbound policy took another load's manifest")
	}
	if _, ok := BoundOrNone(Saferus, ""); ok {
		t.Fatal("a reference policy accepted another load's installed manifest")
	}
	if got, ok := BoundOrNone("example/other", ""); !ok || got.Repository != "" {
		t.Fatal("a manifest naming another repository changed nothing for this one")
	}
	other := m
	other.BaseBranch = "develop"
	SetInstalled(&other)
	if _, ok := BoundOrNone("example/service", digest); ok {
		t.Fatal("another load's manifest accepted")
	}
}
