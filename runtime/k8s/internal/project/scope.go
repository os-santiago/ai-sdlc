// Package project defines the reviewed delivery targets. A matching
// repository is necessary but never sufficient authority for a workflow effect.
// Installed policy, signed receipts and current evidence remain mandatory.
package project

import "sync/atomic"

const (
	Homedir = "os-santiago/homedir"
	Saferus = "os-santiago/saferus"
)

// installed is the manifest bound by the most recently authenticated policy
// (its ProjectSHA256), or nil when the policy selects a built-in reference.
var installed atomic.Pointer[Manifest]

// SetInstalled records the manifest resolved from an authenticated policy.
// Callers pass nil when the policy binds no manifest.
func SetInstalled(m *Manifest) {
	if m == nil {
		installed.Store(nil)
		return
	}
	copied := *m
	installed.Store(&copied)
}

// Supported reports whether the runtime may deliver to a repository: a
// built-in reference, or the repository of the policy-bound manifest.
func Supported(repository string) bool {
	if repository == Homedir || repository == Saferus {
		return true
	}
	m := installed.Load()
	return m != nil && m.Repository == repository
}

// Bound returns the manifest that one specific policy binds for a
// repository: the installed manifest with exactly that policy's
// ProjectSHA256, or the built-in reference when the policy binds none.
// Unlike Lookup it never substitutes the manifest of another policy load,
// so a concurrent reload that fails (clearing the installed manifest) or
// installs a different manifest refuses instead of changing the scope or
// commit identity of work authorized under the original policy.
func Bound(repository, projectSHA256 string) (Manifest, bool) {
	m := installed.Load()
	if projectSHA256 == "" {
		if m != nil {
			return Manifest{}, false
		}
		return Builtin(repository)
	}
	if _, reference := Builtin(repository); reference || m == nil || m.Repository != repository || m.Digest() != projectSHA256 {
		return Manifest{}, false
	}
	return *m, true
}

// Lookup returns the manifest governing a repository: the policy-bound
// manifest when it names the repository, else a built-in reference.
func Lookup(repository string) (Manifest, bool) {
	if m := installed.Load(); m != nil && m.Repository == repository {
		return *m, true
	}
	return Builtin(repository)
}
