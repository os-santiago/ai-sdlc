package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestColdRegistryCustodyUsesSeparateStrongCredentials(t *testing.T) {
	var output bytes.Buffer
	if err := createRegistryCustody(strings.NewReader(`{"version":"ai-sdlc.cold-registry-custody/v1"}`), &output); err != nil {
		t.Fatal(err)
	}
	var actual coldRegistryCustody
	if json.Unmarshal(output.Bytes(), &actual) != nil || actual.Version != coldRegistryCustodyVersion || actual.Publisher.Username != "konflux" || actual.Reader.Username != "ai-sdlc-verifier" || actual.Reader.Password == actual.Publisher.Password {
		t.Fatal("invalid custody shape")
	}
	lines := strings.Split(strings.TrimSuffix(actual.HTPasswd, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatal("unexpected registry users")
	}
	for i, credential := range []registryCredential{actual.Publisher, actual.Reader} {
		if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(credential.Password) {
			t.Fatal("weak password encoding")
		}
		hash := strings.TrimPrefix(lines[i], credential.Username+":")
		cost, err := bcrypt.Cost([]byte(hash))
		if err != nil || cost != 12 || bcrypt.CompareHashAndPassword([]byte(hash), []byte(credential.Password)) != nil {
			t.Fatal("invalid credential hash")
		}
	}
	var docker struct {
		Auths map[string]map[string]string `json:"auths"`
	}
	if json.Unmarshal(actual.DockerConfig, &docker) != nil || len(docker.Auths) != 1 {
		t.Fatal("invalid publisher configuration")
	}
	entry := docker.Auths["registry-service.kind-registry"]
	decoded, err := base64.StdEncoding.DecodeString(entry["auth"])
	if err != nil || string(decoded) != actual.Publisher.Username+":"+actual.Publisher.Password || entry["password"] != actual.Publisher.Password || entry["username"] != "konflux" {
		t.Fatal("publisher configuration mismatch")
	}
}

func TestColdRegistryCustodyRejectsAdoptionAndUnknownFields(t *testing.T) {
	for _, raw := range []string{`{}`, `null`, `[]`, `{"version":"other"}`, `{"Version":"ai-sdlc.cold-registry-custody/v1"}`,
		`{"version":"ai-sdlc.cold-registry-custody/v1","existingPassword":"private-sentinel"}`,
		`{"version":"ai-sdlc.cold-registry-custody/v1","version":"ai-sdlc.cold-registry-custody/v1"}`, strings.Repeat(" ", 4097)} {
		var output bytes.Buffer
		if err := createRegistryCustody(strings.NewReader(raw), &output); err != errProvenanceInstall || output.Len() != 0 {
			t.Fatal("invalid input generated or exposed custody")
		}
	}
}
