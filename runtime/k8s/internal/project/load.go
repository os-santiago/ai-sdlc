package project

import (
	"io"
	"os"

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
	m, err := ParseManifest(installed)
	if err != nil || m.Repository != policy.Repository || m.Digest() != policy.ProjectSHA256 {
		return Manifest{}, ErrManifest
	}
	return m, nil
}

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
	f, err := os.Open(path)
	if err != nil {
		return Manifest{}, ErrManifest
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, MaxManifestBytes+1))
	if err != nil {
		return Manifest{}, ErrManifest
	}
	return Resolve(policy, raw)
}
