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

// Lookup returns the manifest governing a repository: the policy-bound
// manifest when it names the repository, else a built-in reference.
func Lookup(repository string) (Manifest, bool) {
	if m := installed.Load(); m != nil && m.Repository == repository {
		return *m, true
	}
	return Builtin(repository)
}
