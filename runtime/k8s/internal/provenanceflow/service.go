package provenanceflow

import (
	"context"
	"net/http"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/provenance"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

// Service runs in the dedicated provenance process using the verifier image and
// identity. It has no model, candidate execution or Kubernetes cleanup dependency.
// The installer routes verify-built only to this process's Service.
type Service struct {
	DB     *store.DB
	Mounts Mounts
}

func (s Service) Load() (provenance.Configuration, error) {
	if s.DB == nil {
		return provenance.Configuration{}, ErrFlow
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if blocked, e := s.DB.ProvenanceContainmentBlocked(ctx); e != nil || blocked {
		return provenance.Configuration{}, ErrFlow
	}
	cfg, _, e := s.Mounts.Load("verifier")
	return cfg, e
}
func (s Service) Identity() (authority.Policy, authority.Identity, error) {
	cfg, e := s.Load()
	if e != nil {
		return authority.Policy{}, authority.Identity{}, e
	}
	current, key, e := s.Mounts.Load("verifier")
	if e != nil || cfg.Trust.Digest() != current.Trust.Digest() || cfg.Build.Outer.Revision() != current.Build.Outer.Revision() {
		return authority.Policy{}, authority.Identity{}, ErrFlow
	}
	return current.Build.Outer, key, nil
}
func (s Service) Ready(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if s.DB == nil || store.AssertComponent(ctx, s.DB.Pool, "verifier") != nil || s.DB.CheckMigrations(ctx) != nil {
		return ErrFlow
	}
	cfg, e := s.Load()
	if e != nil {
		return e
	}
	_, e = s.Mounts.Verifier(cfg)
	return e
}
func (s Service) Handler() http.Handler {
	mux := http.NewServeMux()
	inbox := delivery.Inbox{DB: s.DB, Target: "verifier"}
	mux.Handle("POST /internal/verify-built", inbox.Handler(s.Identity, "verify-built", "/internal/verify-built", InboxEffect(store.NewProvenanceJournal(s.DB, s.Load))))
	return mux
}
func (s Service) PollOnce(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 75*time.Second)
	defer cancel()
	return (Runtime{DB: s.DB, Load: s.Load, Verifier: s.Mounts.Verifier}).PollOnce(ctx)
}
