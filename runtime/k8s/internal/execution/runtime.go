package execution

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/intake"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelbroker"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/bootstrap"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/sourcefetch"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// RuntimeConfig is installation-owned. It cannot contain shell commands,
// credentials, arbitrary mounts or a forge publication capability.
type RuntimeConfig struct {
	Version   string           `json:"version"`
	Profile   executor.Profile `json:"profile"`
	ADev      string           `json:"aDev"`
	Placement Placement        `json:"placement"`
	Model     ModelPolicy      `json:"model"`
}

func ReadBounded(path string, limit int64) ([]byte, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, ErrAuthority
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, limit+1))
	if e != nil || len(b) == 0 || int64(len(b)) > limit {
		return nil, ErrAuthority
	}
	return b, nil
}
func LoadRuntime(path string) (RuntimeConfig, []byte, error) {
	var c RuntimeConfig
	b, e := ReadBounded(path, 128*1024)
	if e != nil || authority.DecodeStrict(b, &c) != nil || c.Version != "ai-sdlc.candidate-runtime/v1" || !c.Model.valid() || len(c.ADev) == 0 || len(c.ADev) > 65536 {
		return c, nil, ErrAuthority
	}
	profile, _ := json.Marshal(c.Profile)
	if _, e = executor.ParseProfile(profile, workspacebundle.Digest(profile)); e != nil || c.Profile.ADevSHA256 != workspacebundle.Digest([]byte(c.ADev)) {
		return c, nil, ErrAuthority
	}
	if c.Placement.Namespace != "ai-sdlc-executors" || c.Profile.Model != "codex-cli" || executor.ValidatePlacement(c.Placement.Namespace, c.Placement.WorkerImage, c.Placement.MailboxImage) != nil {
		return c, nil, ErrAuthority
	}
	// Immutable placement identities are checked before any queue operation.
	return c, profile, nil
}

// Runtime polls only durable requests received by the signed worker inbox.
// It has no externally callable run/claim/advance endpoint and no retry/adoption.
type Runtime struct {
	SourceCache                                                 *sourcefetch.Cache
	Component                                                   *bootstrap.Component
	ConfigPath, IntakePath, TokenPath, CAPath, GatewayTokenPath string
	APIServer, ProviderEndpoint                                 string
	ProviderTimeoutSeconds                                      int
	// AdoptGatewayProfile makes every model call use the timeout the gateway
	// derived from its startup benchmark (+30 s for the response), bounded by
	// ProviderTimeoutSeconds. No work is claimed until that profile exists.
	AdoptGatewayProfile bool
}

func (r Runtime) PollOnce(ctx context.Context) (string, error) {
	if r.Component == nil || r.Component.DB == nil || r.Component.Name != "worker" {
		return "", ErrAuthority
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	config, _, err := LoadRuntime(r.ConfigPath)
	if err != nil {
		return "", err
	}
	original, _ := json.Marshal(config)
	load := func(ctx context.Context) (PreparationInputs, Placement, error) {
		p, _, e := r.Component.Authority()
		if e != nil || !bootstrap.CandidateAutonomy(p) || p.Freeze {
			return PreparationInputs{}, Placement{}, ErrAuthority
		}
		current, raw, e := LoadRuntime(r.ConfigPath)
		sealed, _ := json.Marshal(current)
		if e != nil || workspacebundle.Digest(sealed) != workspacebundle.Digest(original) || ctx.Err() != nil {
			return PreparationInputs{}, Placement{}, ErrAuthority
		}
		cfg, e := intake.LoadConfig(r.IntakePath, p.Repository)
		if e != nil {
			return PreparationInputs{}, Placement{}, ErrAuthority
		}
		return PreparationInputs{Policy: p, Intake: cfg, Profile: raw, ADev: []byte(current.ADev)}, current.Placement, nil
	}
	// Credentials stay outside the executor Pod and are never placed in artifacts.
	token, e := ReadBounded(r.TokenPath, 8192)
	if e != nil {
		return "", e
	}
	ca, e := ReadBounded(r.CAPath, 65536)
	if e != nil {
		return "", e
	}
	client, e := podtransport.NewClient(podtransport.Config{APIServer: r.APIServer, CA: ca, BearerToken: string(token)})
	if e != nil {
		return "", ErrAuthority
	}
	// Terminal cleanup runs even if new work is frozen or the model is unavailable.
	if e = r.Component.DB.ExpireQueuedExecutions(ctx); e != nil {
		return "", e
	}
	if e = r.Component.DB.ExpireExecutionClaims(ctx); e != nil {
		return "", e
	}
	cleanup, e := r.Component.DB.NextExecutionCleanup(ctx)
	if e != nil {
		return "", e
	}
	if cleanup.RequestID != "" {
		removed := client.CleanupAcknowledged(ctx, cleanup.Namespace, cleanup.PodName, cleanup.ConfigName, cleanup.PodUID, cleanup.ConfigUID) == nil
		if e = r.Component.DB.FinishExecutionCleanup(ctx, cleanup, removed); e != nil || !removed {
			return "cleanup-pending", ErrAuthority
		}
	}
	blocked, e := r.Component.DB.ExecutionCleanupBlocked(ctx)
	if e != nil || blocked {
		return "cleanup-uncertain", ErrAuthority
	}
	if _, _, e = load(ctx); e != nil {
		return "", e
	}
	if os.Getenv("AI_SDLC_RUNTIME_MODE") == "verification" {
		sent, e := HandoffCandidate(ctx, r.Component.DB, func() (authority.Policy, authority.Identity, intake.Config, string, error) {
			v, _, e := load(ctx)
			if e != nil {
				return authority.Policy{}, authority.Identity{}, intake.Config{}, "", e
			}
			p, i, e := r.Component.Authority()
			if e != nil || p.Revision() != v.Policy.Revision() {
				return authority.Policy{}, authority.Identity{}, intake.Config{}, "", ErrAuthority
			}
			return p, i, v.Intake, workspacebundle.Digest(v.Profile), nil
		})
		if e != nil {
			return "verification-handoff-stopped", e
		}
		if sent {
			return "verification-dispatched", nil
		}
	}
	provider, e := r.gateway()
	if e != nil || provider.CheckGateway(ctx) != nil {
		return "", ErrAuthority
	}
	if provider, e = r.benchmarked(ctx, provider, config.Profile.CallWindow()); e != nil {
		return "model-benchmark-pending", ErrAuthority
	}
	id, e := r.Component.DB.NextExecution(ctx)
	if e != nil || id == "" {
		return "", e
	}
	ready := func() (authority.Policy, intake.Config, error) { v, _, e := load(ctx); return v.Policy, v.Intake, e }
	cache := r.SourceCache
	if cache == nil {
		cache = sourcefetch.NewCache()
	}
	source, e := acquireQueuedSource(ctx, r.Component.DB, func(ctx context.Context) (PreparationInputs, error) { v, _, e := load(ctx); return v, e }, id, cache.Fetch)
	if e != nil {
		return "source-deferred", e
	}
	claim, e := Claim(ctx, r.Component.DB, ready, id)
	if e != nil {
		_ = r.Component.DB.RejectQueuedExecution(ctx, id)
		return "rejected", ErrAuthority
	}
	prep, e := prepare(ctx, r.Component.DB, func(ctx context.Context) (PreparationInputs, error) { v, _, e := load(ctx); return v, e }, claim, func(ctx context.Context, repository, sha string) (workspacebundle.Bundle, error) {
		if ctx.Err() != nil || repository != source.repository || sha != source.bundle.SourceSHA {
			return workspacebundle.Bundle{}, ErrAuthority
		}
		return source.bundle, nil
	})
	if e != nil {
		return "stopped", e
	}
	_, e = RunController(ctx, r.Component.DB, load, prep, client, provider, func(ctx context.Context) (ModelPolicy, error) { _, _, e := load(ctx); return config.Model, e })
	if e != nil {
		return "stopped", e
	}
	return "candidate-captured", nil
}

// Ready observes prerequisites only; it never claims work or invokes a model.
func (r Runtime) Ready(ctx context.Context) error {
	if _, _, e := LoadRuntime(r.ConfigPath); e != nil {
		return ErrAuthority
	}
	blocked, e := r.Component.DB.ExecutionCleanupBlocked(ctx)
	if e != nil || blocked {
		return ErrAuthority
	}
	provider, e := r.gateway()
	if e != nil {
		return ErrAuthority
	}
	return provider.CheckGateway(ctx)
}

// benchmarked returns a provider bounded by the gateway's measured timeout.
// The measured upstream timeout must also fit the profile's per-call window:
// the broker stops 15 s before the window ends, so a slower model would only
// produce uncertain calls. Such a worker claims no work (fail closed).
func (r Runtime) benchmarked(ctx context.Context, provider *modelbroker.Provider, window time.Duration) (*modelbroker.Provider, error) {
	if !r.AdoptGatewayProfile {
		return provider, nil
	}
	seconds, e := provider.GatewayProfile(ctx)
	if e != nil || r.ProviderTimeoutSeconds == 0 || seconds+30 > r.ProviderTimeoutSeconds || time.Duration(seconds+20)*time.Second > window {
		return nil, ErrAuthority
	}
	r.ProviderTimeoutSeconds = seconds + 30
	return r.gateway()
}

func (r Runtime) gateway() (*modelbroker.Provider, error) {
	secret, e := ReadBounded(r.GatewayTokenPath, 512)
	if e != nil {
		return nil, e
	}
	endpoint := r.ProviderEndpoint
	if endpoint == "" {
		endpoint = "http://127.0.0.1:8787/v1/chat/completions"
	}
	return modelbroker.NewProvider(modelbroker.ProviderConfig{Endpoint: endpoint, BearerToken: string(secret), AllowLoopbackHTTP: true, TimeoutSeconds: r.ProviderTimeoutSeconds})
}
