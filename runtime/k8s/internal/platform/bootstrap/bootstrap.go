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
	if mode != "" && mode != "maintenance" && mode != "shadow" {
		return errors.New("runtime supports maintenance and bounded shadow modes only")
	}
	return nil
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
	raw, err := boundedFile(c.PolicyPath)
	if err != nil {
		return authority.Policy{}, authority.Identity{}, err
	}
	p, err := authority.ParsePolicy(raw)
	if err != nil {
		return authority.Policy{}, authority.Identity{}, err
	}
	if os.Getenv("AI_SDLC_RUNTIME_MODE") == "shadow" && p.Autonomy != "shadow" {
		return authority.Policy{}, authority.Identity{}, errors.New("shadow mode requires shadow authority ceiling")
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
