package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildworkload"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/provenance"
	"golang.org/x/crypto/bcrypt"
)

var errProvenanceInstall = errors.New("provenance installation input denied")

func exactInstallFields(raw []byte, required, optional []string) bool {
	var fields map[string]json.RawMessage
	if authority.DecodeStrict(raw, &fields) != nil {
		return false
	}
	allowed := map[string]bool{}
	for _, key := range required {
		if _, present := fields[key]; !present {
			return false
		}
		allowed[key] = true
	}
	for _, key := range optional {
		allowed[key] = true
	}
	for key := range fields {
		if !allowed[key] {
			return false
		}
	}
	return true
}

func canonicalInstallValue(raw []byte, value any) bool {
	encoded, err := json.Marshal(value)
	if err != nil {
		return false
	}
	var original, canonical any
	return json.Unmarshal(raw, &original) == nil && json.Unmarshal(encoded, &canonical) == nil && reflect.DeepEqual(original, canonical)
}

// This compiler validates separately approved public inputs. It cannot observe
// a cluster, approve an observed key, generate signing authority or advance work.
func compileProvenanceInstallation(input io.Reader, output io.Writer) error {
	raw, err := io.ReadAll(io.LimitReader(input, 256*1024+1))
	if err != nil || len(raw) > 256*1024 {
		return errProvenanceInstall
	}
	var request struct {
		Trust         json.RawMessage       `json:"trust"`
		Profile       buildworkload.Profile `json:"profile"`
		VerifierImage string                `json:"verifierImage"`
	}
	if !exactInstallFields(raw, []string{"trust", "profile", "verifierImage"}, nil) || authority.DecodeStrict(raw, &request) != nil || request.Profile.Validate() != nil {
		return errProvenanceInstall
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	if !canonicalInstallValue(fields["profile"], request.Profile) {
		return errProvenanceInstall
	}
	trust, err := provenance.ParseTrust(request.Trust, time.Now())
	if err != nil || trust.ClusterUID != request.Profile.ClusterUID || trust.BuildNamespaceUID != request.Profile.NamespaceUID ||
		trust.PublisherImage != request.Profile.PublisherImage || trust.VerifierImage != request.VerifierImage ||
		!regexp.MustCompile(`^quay\.io/os-santiago/ai-sdlc@sha256:[a-f0-9]{64}$`).MatchString(trust.PublisherImage) {
		return errProvenanceInstall
	}
	return json.NewEncoder(output).Encode(struct {
		Version       string           `json:"version"`
		Trust         provenance.Trust `json:"trust"`
		TrustSHA256   string           `json:"trustSHA256"`
		ProfileSHA256 string           `json:"profileSHA256"`
		VerifierImage string           `json:"verifierImage"`
	}{"ai-sdlc.provenance-installation/v1", trust, trust.Digest(), request.Profile.Digest(), request.VerifierImage})
}

type registryCredential struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type registryCustody struct {
	Version    string             `json:"version"`
	Credential registryCredential `json:"credential"`
	ReaderHash string             `json:"readerHash"`
	HTPasswd   string             `json:"htpasswd"`
}

const custodyVersion = "ai-sdlc.provenance-registry-custody/v1"

// Private stdin/stdout only. The installer must durably persist this output
// BEFORE changing registry htpasswd. Retry supplies exactly that saved material;
// this operation has no database, network, file or installation side effects.
func prepareProvenanceRegistryCustody(input io.Reader, output io.Writer) error {
	raw, err := io.ReadAll(io.LimitReader(input, 4097))
	if err != nil || len(raw) > 4096 {
		return errProvenanceInstall
	}
	var request struct {
		Version    string              `json:"version"`
		HTPasswd   string              `json:"htpasswd"`
		Credential *registryCredential `json:"credential"`
		ReaderHash string              `json:"readerHash"`
	}
	if !exactInstallFields(raw, []string{"version", "htpasswd"}, []string{"credential", "readerHash"}) ||
		authority.DecodeStrict(raw, &request) != nil || request.Version != custodyVersion ||
		!strings.HasSuffix(request.HTPasswd, "\n") || strings.ContainsAny(request.HTPasswd, "\r\x00") {
		return errProvenanceInstall
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	_, credentialProvided := fields["credential"]
	_, hashProvided := fields["readerHash"]
	if credentialProvided != hashProvided || credentialProvided && request.Credential == nil {
		return errProvenanceInstall
	}
	lines := strings.Split(strings.TrimSuffix(request.HTPasswd, "\n"), "\n")
	if len(lines) < 1 || len(lines) > 2 || !strings.HasPrefix(lines[0], "konflux:") {
		return errProvenanceInstall
	}
	publisher := strings.TrimPrefix(lines[0], "konflux:")
	validHash := func(hash string) bool {
		if !regexp.MustCompile(`^\$2[aby]\$[0-9]{2}\$[./A-Za-z0-9]{53}$`).MatchString(hash) {
			return false
		}
		cost, e := bcrypt.Cost([]byte(hash))
		return e == nil && cost >= 10 && cost <= 14
	}
	if !validHash(publisher) {
		return errProvenanceInstall
	}
	var credential registryCredential
	readerHash := request.ReaderHash
	if request.Credential == nil {
		// An existing reader without its recorded credential is ambiguous. Do not
		// rotate it or infer ownership by username.
		if len(lines) != 1 || readerHash != "" {
			return errProvenanceInstall
		}
		password := make([]byte, 32)
		if _, err := rand.Read(password); err != nil {
			return errProvenanceInstall
		}
		credential = registryCredential{"ai-sdlc-verifier", hex.EncodeToString(password)}
		hash, err := bcrypt.GenerateFromPassword([]byte(credential.Password), 12)
		if err != nil {
			return errProvenanceInstall
		}
		readerHash = string(hash)
	} else {
		if !exactInstallFields(fields["credential"], []string{"username", "password"}, nil) {
			return errProvenanceInstall
		}
		credential = *request.Credential
		cost, err := bcrypt.Cost([]byte(readerHash))
		if credential.Username != "ai-sdlc-verifier" || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(credential.Password) ||
			!validHash(readerHash) || err != nil || cost != 12 || bcrypt.CompareHashAndPassword([]byte(readerHash), []byte(credential.Password)) != nil {
			return errProvenanceInstall
		}
	}
	if len(lines) == 2 && lines[1] != "ai-sdlc-verifier:"+readerHash {
		return errProvenanceInstall
	}
	// Preserve the original publisher line byte-for-byte. No administrator,
	// additional users, weaker hash or silent credential rotation is admitted.
	combined := lines[0] + "\nai-sdlc-verifier:" + readerHash + "\n"
	return json.NewEncoder(output).Encode(registryCustody{custodyVersion, credential, readerHash, combined})
}
