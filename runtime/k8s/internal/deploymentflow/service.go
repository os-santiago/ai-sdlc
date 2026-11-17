package deploymentflow

import (
	"context"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releaseflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"net/http"
	"time"
)

type Service struct {
	DB        *store.DB
	Mounts    releaseflow.Mounts
	Transport func(releasing.Target) (Transport, error)
	Cleaner   func() (BrowserCleaner, error)
}

func (s Service) Load() (releasing.Configuration, error) {
	cfg, _, e := s.Mounts.Load("deployer")
	return cfg, e
}
func (s Service) Identity() (authority.Policy, authority.Identity, error) {
	cfg, key, e := s.Mounts.Load("deployer")
	if e != nil {
		return authority.Policy{}, authority.Identity{}, e
	}
	return cfg.Provenance.Build.Outer, key, nil
}
func (s Service) Ready(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if s.DB == nil || s.Transport == nil || store.AssertComponent(ctx, s.DB.Pool, "deployer") != nil || s.DB.CheckMigrations(ctx) != nil {
		return ErrFlow
	}
	_, e := s.Load()
	return e
}
func (s Service) Handler() http.Handler {
	inbox := delivery.Inbox{DB: s.DB, Target: "deployer"}
	return inbox.Handler(s.Identity, "deploy-release", "/internal/deploy-release", InboxEffect(store.NewDeploymentJournalWithTarget(s.DB, s.Load, s.Mounts.LoadTarget, s.Mounts.LoadBrowser)))
}
func (s Service) PollOnce(ctx context.Context) (string, error) {
	return (Runtime{DB: s.DB, Load: s.Load, Target: s.Mounts.LoadTarget, Browser: s.Mounts.LoadBrowser, Transport: s.Transport, Cleaner: s.Cleaner, CleanupTarget: s.Mounts.LoadTargetIdentity}).PollOnce(ctx)
}
