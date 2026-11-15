package authority

import "testing"

func TestBootstrapIsFrozenAndSeparatesEveryIdentity(t *testing.T) {
	material, err := Bootstrap("example/project", "production-test")
	if err != nil {
		t.Fatal(err)
	}
	if !material.Policy.Freeze || material.Policy.Autonomy != "shadow" {
		t.Fatal("bootstrap granted delivery authority")
	}
	if err := material.Validate(); err != nil {
		t.Fatal(err)
	}
	for role, identity := range material.Identities {
		if identity.Role != role {
			t.Fatal("identity role mismatch")
		}
		if err := identity.Validate(material.Policy); err != nil {
			t.Fatal(err)
		}
	}
	delete(material.Identities, "worker")
	if err := material.Validate(); err == nil {
		t.Fatal("incomplete identity set accepted")
	}
}
