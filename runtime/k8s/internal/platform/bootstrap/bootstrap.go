// Package bootstrap loads trusted mounted identities and checks durable service
// dependencies. Ordinary components never migrate schemas or provision roles.
package bootstrap

import (
	"context"
	"errors"
	"io"
	"os"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

type Component struct {
	Name         string
	DB           *store.DB
	PolicyPath   string
	IdentityPath string
}

// ValidateMode deliberately keeps delivery closed while its downstream
// executors and release gates are being integrated. Unknown values never mean
// active. An omitted mode is maintenance, not a stateless public evaluator.
func ValidateMode() error {
	mode := os.Getenv("AI_SDLC_RUNTIME_MODE")
	if mode != "" && mode != "maintenance" && mode != "shadow" && mode != "candidate" && mode != "verification" {
		return errors.New("runtime supports maintenance, shadow, candidate and verification modes only")
	}
	option := os.Getenv("AI_SDLC_PUBLICATION")
	if option != "" && option != "false" && option != "true" {
		return errors.New("invalid publication option")
	}
	if option == "true" && (mode != "verification" || os.Getenv("AI_SDLC_INDEPENDENT_REVIEW") != "true") {
		return errors.New("publication requires verification and independent review")
	}
	merge := os.Getenv("AI_SDLC_MERGE")
	if merge != "" && merge != "false" && merge != "true" {
		return errors.New("invalid merge option")
	}
	if merge == "true" && option != "true" {
		return errors.New("merge requires verified publication")
	}
	build := os.Getenv("AI_SDLC_BUILD")
	if build != "" && build != "false" && build != "true" {
		return errors.New("invalid build option")
	}
	if build == "true" && merge != "true" {
		return errors.New("build requires verified merge")
	}
	provenance := os.Getenv("AI_SDLC_PROVENANCE")
	if provenance != "" && provenance != "false" && provenance != "true" {
		return errors.New("invalid provenance option")
	}
	if provenance == "true" && build != "true" {
		return errors.New("provenance requires verified build")
	}
	release := os.Getenv("AI_SDLC_RELEASE")
	if release != "" && release != "false" && release != "true" {
		return errors.New("invalid release option")
	}
	if release == "true" && provenance != "true" {
		return errors.New("release requires verified provenance")
	}
	return nil
}
func ReleaseEnabled() bool { return ValidateMode() == nil && os.Getenv("AI_SDLC_RELEASE") == "true" }

func ProvenanceEnabled() bool {
	return ValidateMode() == nil && os.Getenv("AI_SDLC_PROVENANCE") == "true"
}

func MergeEnabled() bool { return ValidateMode() == nil && os.Getenv("AI_SDLC_MERGE") == "true" }
func BuildEnabled() bool { return ValidateMode() == nil && os.Getenv("AI_SDLC_BUILD") == "true" }
func CandidateAutonomy(p authority.Policy) bool {
	if MergeEnabled() {
		return project.Supported(p.Repository) && p.Autonomy == "auto-merge-low"
	}
	return p.Autonomy == "auto-pr"
}

// PublicationEnabled is a trusted capability ceiling, not a route or grant.
// No installed component activates a writer merely by setting this option.
func PublicationEnabled() bool {
	return ValidateMode() == nil && os.Getenv("AI_SDLC_PUBLICATION") == "true"
}

func Open(ctx context.Context, name string) (*Component, error) {
	if err := ValidateMode(); err != nil {
		return nil, err
	}
	if _, err := store.ComponentRole(name); err != nil {
		return nil, err
	}
	if os.Getenv("DATABASE_URL") == "" {
		return nil, errors.New("dedicated component database credential required")
	}
	db, err := store.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return nil, errors.New("component database unavailable")
	}
	c := &Component{Name: name, DB: db, PolicyPath: os.Getenv("AI_SDLC_AUTHORITY_POLICY_PATH"), IdentityPath: os.Getenv("AI_SDLC_IDENTITY_PATH")}
	if err := c.Ready(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return c, nil
}

// ProjectManifestPath is the read-only mount of the reviewed project manifest.
func ProjectManifestPath() string {
	if path := os.Getenv("AI_SDLC_PROJECT_MANIFEST_PATH"); path != "" {
		return path
	}
	return "/etc/ai-sdlc/project/manifest.json"
}

func boundedFile(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("trusted configuration path required")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("trusted configuration unavailable")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 128*1024+1))
	if err != nil || len(raw) > 128*1024 {
		return nil, errors.New("trusted configuration exceeds size limit")
	}
	return raw, nil
}

func (c *Component) Authority() (authority.Policy, authority.Identity, error) {
	if err := ValidateMode(); err != nil {
		return authority.Policy{}, authority.Identity{}, err
	}
	raw, err := boundedFile(c.PolicyPath)
	if err != nil {
		return authority.Policy{}, authority.Identity{}, err
	}
	p, err := authority.ParsePolicy(raw)
	if err != nil {
		return authority.Policy{}, authority.Identity{}, err
	}
	// The manifest is resolved from the same authenticated policy on every
	// load, so delivery scope always follows the current reviewed policy.
	// The installed manifest changes only when the whole load succeeds; any
	// refusal clears it, so no stale scope survives a failed reload.
	var installed *project.Manifest
	loaded := false
	defer func() {
		if loaded {
			project.SetInstalled(installed)
		} else {
			project.SetInstalled(nil)
		}
	}()
	if p.ProjectSHA256 != "" {
		manifest, err := project.Load(ProjectManifestPath(), p)
		if err != nil {
			return authority.Policy{}, authority.Identity{}, errors.New("project manifest does not match the installed policy")
		}
		installed = &manifest
	} else {
		// A mounted manifest the policy does not bind is a configuration
		// error; without either, built-in references keep their behaviour.
		if _, err := os.Lstat(ProjectManifestPath()); err == nil || !os.IsNotExist(err) {
			return authority.Policy{}, authority.Identity{}, errors.New("project manifest is mounted but not bound by the policy")
		}
	}
	if os.Getenv("AI_SDLC_RUNTIME_MODE") == "shadow" && p.Autonomy != "shadow" {
		return authority.Policy{}, authority.Identity{}, errors.New("shadow mode requires shadow authority ceiling")
	}
	if CandidateEnabled() {
		if !CandidateAutonomy(p) {
			return authority.Policy{}, authority.Identity{}, errors.New("candidate authority ceiling does not match installed capabilities")
		}
		for _, grant := range p.Grants {
			if grant.Operation != "admit-issue" && grant.Operation != "plan-admission" && grant.Operation != "execute-work" && !(os.Getenv("AI_SDLC_RUNTIME_MODE") == "verification" && grant.Operation == "verify-artifact") && !(os.Getenv("AI_SDLC_RUNTIME_MODE") == "verification" && os.Getenv("AI_SDLC_INDEPENDENT_REVIEW") == "true" && (grant.Operation == "repair-candidate" || grant.Operation == "stop-rejected-work")) && !(PublicationEnabled() && grant.Operation == "publish-candidate") && !(MergeEnabled() && grant.Operation == "merge-publication") {
				return authority.Policy{}, authority.Identity{}, errors.New("candidate mode denies downstream authority")
			}
		}
	}
	raw, err = boundedFile(c.IdentityPath)
	if err != nil {
		return authority.Policy{}, authority.Identity{}, err
	}
	var identity authority.Identity
	if err := authority.DecodeStrict(raw, &identity); err != nil {
		return authority.Policy{}, authority.Identity{}, err
	}
	if identity.Role != c.Name || identity.Validate(p) != nil {
		return authority.Policy{}, authority.Identity{}, errors.New("component signing identity mismatch")
	}
	loaded = true
	return p, identity, nil
}

func (c *Component) Ready(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, _, err := c.Authority(); err != nil {
		return err
	}
	if c.DB == nil {
		return errors.New("component database required")
	}
	if err := store.AssertComponent(ctx, c.DB.Pool, c.Name); err != nil {
		return err
	}
	return c.DB.CheckMigrations(ctx)
}

// IntakeEnabled installs only authenticated issue ingress and component routes.
func IntakeEnabled() bool {
	m := os.Getenv("AI_SDLC_RUNTIME_MODE")
	return m == "shadow" || m == "candidate" || m == "verification"
}

// CandidateEnabled includes the explicitly enabled independent verification stage.
func CandidateEnabled() bool {
	m := os.Getenv("AI_SDLC_RUNTIME_MODE")
	return m == "candidate" || m == "verification"
}
