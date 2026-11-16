package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"golang.org/x/crypto/bcrypt"
)

const coldRegistryCustodyVersion = "ai-sdlc.cold-registry-custody/v1"

type coldRegistryCustody struct {
	Version      string             `json:"version"`
	Publisher    registryCredential `json:"publisher"`
	Reader       registryCredential `json:"reader"`
	HTPasswd     string             `json:"htpasswd"`
	DockerConfig json.RawMessage    `json:"dockerConfig"`
}

// Private protocol for a NEW Helm-owned registry. Existing installations must
// reuse protected custody. This operation never replaces or adopts credentials,
// contacts Kubernetes or confers authority to change an existing registry.
func createRegistryCustody(input io.Reader, output io.Writer) error {
	raw, err := io.ReadAll(io.LimitReader(input, 4097))
	var request struct {
		Version string `json:"version"`
	}
	if err != nil || len(raw) > 4096 || !exactInstallFields(raw, []string{"version"}, nil) ||
		authority.DecodeStrict(raw, &request) != nil || request.Version != coldRegistryCustodyVersion {
		return errProvenanceInstall
	}
	result := coldRegistryCustody{Version: coldRegistryCustodyVersion}
	for _, destination := range []struct {
		name  string
		value *registryCredential
	}{
		{"konflux", &result.Publisher}, {"ai-sdlc-verifier", &result.Reader},
	} {
		password := make([]byte, 32)
		if _, err = rand.Read(password); err != nil {
			return errProvenanceInstall
		}
		*destination.value = registryCredential{Username: destination.name, Password: hex.EncodeToString(password)}
		hash, err := bcrypt.GenerateFromPassword([]byte(destination.value.Password), 12)
		if err != nil {
			return errProvenanceInstall
		}
		result.HTPasswd += destination.name + ":" + string(hash) + "\n"
	}
	result.DockerConfig, err = json.Marshal(map[string]any{"auths": map[string]any{
		"registry-service.kind-registry": map[string]string{
			"username": result.Publisher.Username, "password": result.Publisher.Password,
			"auth": base64.StdEncoding.EncodeToString([]byte(result.Publisher.Username + ":" + result.Publisher.Password)),
		},
	}})
	if err != nil {
		return errProvenanceInstall
	}
	return json.NewEncoder(output).Encode(result)
}
