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
