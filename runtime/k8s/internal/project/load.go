package project

import (
	"bytes"
	"errors"
	"io"
	"os"
	"syscall"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
)

// Resolve returns the manifest that governs a policy's repository. A policy
// with a ProjectSHA256 accepts only the installed manifest with exactly that
// digest and repository; a policy without one falls back to the built-in
// manifest of a reference repository and refuses any other repository.
func Resolve(policy authority.Policy, installed []byte) (Manifest, error) {
	if policy.ProjectSHA256 == "" {
		if len(installed) != 0 {
			return Manifest{}, ErrManifest
		}
		m, ok := Builtin(policy.Repository)
		if !ok {
			return Manifest{}, ErrManifest
		}
		return m, nil
	}
	// A reference repository is governed only by its reviewed built-in
	// manifest; an installation can never replace it.
	if _, reference := Builtin(policy.Repository); reference {
		return Manifest{}, ErrManifest
	}
	m, err := ParseManifest(installed)
	if err != nil || m.Repository != policy.Repository || m.Digest() != policy.ProjectSHA256 {
		return Manifest{}, ErrManifest
	}
	// The installer always writes the canonical form (its digest is checked
	// over the exact bytes). Requiring it here means every reader of the
	// installed bytes, including the chart's case-sensitive template that
	// sizes the executor quota, sees the same keys the strict but
	// case-insensitive Go decoder accepted.
	if !bytes.Equal(installed, m.Canonical()) {
		return Manifest{}, ErrManifestNotCanonical
	}
	return m, nil
}

// ErrManifestNotCanonical refuses an installed manifest whose bytes are not
// its canonical form, for example with differently capitalized keys.
var ErrManifestNotCanonical = errors.New("installed project manifest is not in canonical form")

// Load reads the installed manifest from a reviewed, read-only mount (never
// from a workspace) and resolves it against the policy. A missing file is
// valid only when the policy selects a built-in manifest.
func Load(path string, policy authority.Policy) (Manifest, error) {
	if policy.ProjectSHA256 == "" {
		// A mounted manifest the policy does not bind is a configuration
		// error, never silently ignored in favour of a built-in.
		if _, err := os.Lstat(path); err == nil || !os.IsNotExist(err) {
			return Manifest{}, ErrManifest
		}
		return Resolve(policy, nil)
	}
	// The mount is a symlink farm, so the path is followed; the content is
	// digest-checked. O_NONBLOCK and the regular-file check keep a FIFO or
	// device from blocking the load.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return Manifest{}, ErrManifest
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return Manifest{}, ErrManifest
	}
	raw, err := io.ReadAll(io.LimitReader(f, MaxManifestBytes+1))
	if err != nil {
		return Manifest{}, ErrManifest
	}
	return Resolve(policy, raw)
}
