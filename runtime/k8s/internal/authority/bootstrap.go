package authority

import (
	"errors"
	"sort"
)

// BootstrapMaterial is private installation output. The installer must capture
// it directly into per-component Secrets, never terminal logs or Git files.
type BootstrapMaterial struct {
	Policy     Policy              `json:"policy"`
	Identities map[string]Identity `json:"identities"`
}

// Bootstrap creates a frozen shadow policy with separate random identities.
// Enabling delivery requires a subsequent reviewed configuration transition.
func Bootstrap(repository, environment string) (BootstrapMaterial, error) {
	material := BootstrapMaterial{Policy: Policy{Version: "1", Repository: repository, Environment: environment, Autonomy: "shadow", Freeze: true, Keys: map[string]PublicKey{}}, Identities: map[string]Identity{}}
	for role := range roles {
		identity, key, err := NewIdentity(role, role+"-1")
		if err != nil {
			return BootstrapMaterial{}, err
		}
		material.Identities[role] = identity
		material.Policy.Keys[identity.KeyID] = key
	}
	names := make([]string, 0, len(operations))
	for operation := range operations {
		names = append(names, operation)
	}
	sort.Strings(names)
	for _, operation := range names {
		pair := operations[operation]
		material.Policy.Grants = append(material.Policy.Grants, Grant{Caller: pair[0], Target: pair[1], Operation: operation, EventType: eventTypes[operation], MinimumLevel: floors[operation]})
	}
	if err := material.Policy.Validate(); err != nil {
		return BootstrapMaterial{}, err
	}
	return material, nil
}

func (m BootstrapMaterial) Validate() error {
	if err := m.Policy.Validate(); err != nil {
		return err
	}
	if len(m.Identities) != len(roles) {
		return errors.New("all component identities required")
	}
	for role := range roles {
		identity, ok := m.Identities[role]
		if !ok || identity.Role != role || identity.Validate(m.Policy) != nil {
			return errors.New("invalid bootstrap identity set")
		}
	}
	return nil
}
