package sourcefetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// CredentialVersion is the only installed read-credential document version.
const CredentialVersion = 1

// maxCredentialBytes bounds the installed document: a 4096-byte token and
// its short, fixed metadata.
const maxCredentialBytes = 8192

var (
	// errCredentialInvalid: the installation declares a read credential, but
	// its document is missing, malformed or bound to another repository.
	errCredentialInvalid = fmt.Errorf("installed source credential invalid: %w", ErrSource)
	// errCredentialRejected: the forge refused the credential (HTTP 401, or
	// 403 that is not a rate limit). The read is never retried anonymously.
	errCredentialRejected = fmt.Errorf("source credential rejected: %w", ErrSource)
)

// CredentialInvalid reports that an installed read credential could not be
// used. The error carries no path content, token or provider response.
func CredentialInvalid(err error) bool { return errors.Is(err, errCredentialInvalid) }

// CredentialRejected reports that the forge refused the read credential.
func CredentialRejected(err error) bool { return errors.Is(err, errCredentialRejected) }

// installedCredential is the Secret key's document. Access must be "read":
// the declaration keeps a write-capable client document (which has other
// fields) from being mounted as a source reader by mistake. The forge, not
// this declaration, enforces the token's real scope.
type installedCredential struct {
	Version    int    `json:"version"`
	Host       string `json:"host"`
	Repository string `json:"repository"`
	Access     string `json:"access"`
	Token      string `json:"token"`
}

// Reader fetches pinned source for one installation. The zero Reader reads
// anonymously, as before. A Reader with a credential path holds only that
// path: the token is read from the mounted file for each fetch (so a rotated
// Secret takes effect without a restart), kept in memory for that fetch only,
// and sent only to the fixed API host for the credential's own repository.
// A credential that cannot be loaded or that the forge rejects stops the
// fetch; it never falls back to an anonymous read.
type Reader struct{ path string }

// InstalledReader returns the anonymous Reader for an empty path, and
// otherwise a Reader bound to the absolute, clean credential file path.
func InstalledReader(path string) (Reader, error) {
	if path == "" {
		return Reader{}, nil
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return Reader{}, errCredentialInvalid
	}
	return Reader{path: path}, nil
}

// Authenticated reports whether the installation configured a read credential.
func (r Reader) Authenticated() bool { return r.path != "" }

// Check loads and validates the installed credential for repository without
// contacting the forge. An anonymous Reader always passes.
func (r Reader) Check(repository string) error {
	_, err := r.client(repository)
	return err
}

// Fetch reads the pinned SHA of repository, authenticated when configured.
func (r Reader) Fetch(ctx context.Context, repository, sha string) (workspacebundle.Bundle, error) {
	client, err := r.client(repository)
	if err != nil {
		return workspacebundle.Bundle{}, err
	}
	return client.Fetch(ctx, sha)
}

func (r Reader) client(repository string) (*Client, error) {
	if r.path == "" {
		return New(repository, "")
	}
	if _, err := New(repository, ""); err != nil {
		return nil, ErrSource
	}
	token, err := loadCredential(r.path, repository)
	if err != nil {
		return nil, errCredentialInvalid
	}
	client, err := New(repository, token)
	if err != nil {
		return nil, errCredentialInvalid
	}
	return client, nil
}

func loadCredential(path, repository string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", errCredentialInvalid
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxCredentialBytes+1))
	if err != nil || len(raw) == 0 || len(raw) > maxCredentialBytes {
		return "", errCredentialInvalid
	}
	var document installedCredential
	if authority.DecodeStrict(raw, &document) != nil || document.Version != CredentialVersion ||
		document.Host != "github.com" || document.Access != "read" || document.Repository != repository || document.Token == "" {
		return "", errCredentialInvalid
	}
	if _, err := New(document.Repository, document.Token); err != nil {
		return "", errCredentialInvalid
	}
	return document.Token, nil
}
