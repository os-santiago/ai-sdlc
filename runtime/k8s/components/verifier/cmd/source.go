package main

import (
	"errors"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/sourcefetch"
)

// sourceCredentialEnvironment names the file of the installation's optional
// read credential. The chart sets it, with the Secret mount, only when
// verifier.sourceCredential is enabled; otherwise source reads stay anonymous.
const sourceCredentialEnvironment = "AI_SDLC_SOURCE_CREDENTIAL_FILE"

// sourceCredentialPath is the only path the chart mounts the credential at.
const sourceCredentialPath = "/var/run/ai-sdlc/source/credential.json"

var errSourceCredential = errors.New("source credential unavailable")

// installedSource returns the verifier's source reader. Only the chart's
// fixed mount path is accepted, so configuration cannot point the reader at
// another mounted Secret.
func installedSource(getenv func(string) string) (sourcefetch.Reader, error) {
	path := getenv(sourceCredentialEnvironment)
	if path != "" && path != sourceCredentialPath {
		return sourcefetch.Reader{}, errSourceCredential
	}
	return sourcefetch.InstalledReader(path)
}

// sourceReady keeps the verifier unready while a configured credential is
// missing, malformed or bound to a repository other than the policy's. It
// never contacts the forge and never reports the credential's content.
func sourceReady(source sourcefetch.Reader, policy func() (authority.Policy, authority.Identity, error)) error {
	if !source.Authenticated() {
		return nil
	}
	p, _, err := policy()
	if err != nil {
		return err
	}
	if source.Check(p.Repository) != nil {
		return errSourceCredential
	}
	return nil
}
