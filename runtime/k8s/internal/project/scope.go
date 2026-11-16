// Package project defines the finite, reviewed delivery targets. A matching
// repository is necessary but never sufficient authority for a workflow effect.
// Installed policy, signed receipts and current evidence remain mandatory.
package project

const (
	Homedir = "os-santiago/homedir"
	Saferus = "os-santiago/saferus"
)

func Supported(repository string) bool {
	return repository == Homedir || repository == Saferus
}
