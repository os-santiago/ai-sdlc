package podtransport

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

var hex64 = regexp.MustCompile(`^[a-f0-9]{64}$`)

var ErrConsoleUncertain = errors.New("console patch acknowledgement is uncertain; quarantine required")

// ConsoleEffectGate is implemented by the owning deployment journal. It checks
// the original durable intent and current authority immediately before PATCH,
// after all potentially blocking observations. It is never model-provided.
type ConsoleEffectGate interface {
	CheckConsoleEffect(context.Context, ConsoleSnapshot, string, bool) error
}
type ConsoleReceipt = releasing.Receipt

func consoleImage(s string) bool {
	return strings.HasPrefix(s, building.Registry+"@sha256:") && len(s) == len(building.Registry+"@sha256:")+64 && hex64.MatchString(strings.TrimPrefix(s, building.Registry+"@sha256:"))
}

// ConsolePatch compiles only tests and a single image replacement. All objects,
// paths and operation choices are compiled, never copied from issue contents.
func ConsolePatch(t releasing.Target, before ConsoleSnapshot, image string) ([]byte, error) {
	if t.Validate(time.Now()) != nil || before.DeploymentUID != t.DeploymentUID || before.TargetSHA256 != t.Digest() || !consoleRV.MatchString(before.ResourceVersion) || before.Generation < 1 || before.Image == image || (!consoleImage(image) && image != t.BaselineImage) {
		return nil, ErrTransport
	}
	patch := []map[string]any{
		{"op": "test", "path": "/metadata/uid", "value": before.DeploymentUID},
		{"op": "test", "path": "/metadata/resourceVersion", "value": before.ResourceVersion},
		{"op": "test", "path": "/metadata/generation", "value": before.Generation},
		{"op": "test", "path": "/spec/template/spec/containers/0/name", "value": "console"},
		{"op": "test", "path": "/spec/template/spec/containers/0/image", "value": before.Image},
		{"op": "replace", "path": "/spec/template/spec/containers/0/image", "value": image},
	}
	raw, e := json.Marshal(patch)
	if e != nil {
		return nil, ErrTransport
	}
	return raw, nil
}

// ConsoleEffects allows one advance and one containment attempt per instance.
// New instances do not renew authority: the durable journal owns that boundary.
type ConsoleEffects struct {
	Deployment      *ConsoleDeployment
	Gate            ConsoleEffectGate
	apply, rollback atomic.Bool
}

func (e *ConsoleEffects) patch(ctx context.Context, before ConsoleSnapshot, image string, rollback bool) (ConsoleReceipt, error) {
	if e == nil || e.Deployment == nil || e.Gate == nil {
		return ConsoleReceipt{}, ErrTransport
	}
	c := e.Deployment
	ctx, cancel := context.WithDeadline(ctx, minDeadline(time.Now().Add(40*time.Second), time.UnixMilli(c.target.ExpiresAt)))
	defer cancel()
	if before.ObservedAt <= 0 || before.ObservedAt > time.Now().Add(5*time.Second).UnixMilli() || before.ObservedAt < time.Now().Add(-30*time.Second).UnixMilli() {
		return ConsoleReceipt{}, ErrTransport
	}
	raw, err := ConsolePatch(c.target, before, image)
	if err != nil {
		return ConsoleReceipt{}, err
	}
	current, err := c.Observe(ctx)
	if err != nil || current.DeploymentUID != before.DeploymentUID || current.ResourceVersion != before.ResourceVersion || current.Generation != before.Generation || current.Image != before.Image || current.TargetSHA256 != before.TargetSHA256 {
		return ConsoleReceipt{}, ErrTransport
	}
	hash := authority.Digest(raw)
	if e.Gate.CheckConsoleEffect(ctx, before, hash, rollback) != nil || ctx.Err() != nil {
		return ConsoleReceipt{}, ErrTransport
	}
	response, err := c.request(ctx, http.MethodPatch, consolePath, raw)
	// Any result after sending the mutating request may have changed the object.
	// Never infer success, retry, or rollback from a matching image alone.
	if err != nil {
		return ConsoleReceipt{}, ErrConsoleUncertain
	}
	after, err := c.deployment(response)
	if err != nil || after.Generation != before.Generation+1 || after.ResourceVersion == before.ResourceVersion || after.Spec.Template.Spec.Containers[0].Image != image {
		return ConsoleReceipt{}, ErrConsoleUncertain
	}
	snap := ConsoleSnapshot{DeploymentUID: string(after.UID), ResourceVersion: after.ResourceVersion, Generation: after.Generation, Image: image, TargetSHA256: c.target.Digest(), ObservedAt: time.Now().UnixMilli()}
	return ConsoleReceipt{Before: before, After: snap, RequestSHA256: hash, Rollback: rollback}, nil
}
func (e *ConsoleEffects) Apply(ctx context.Context, p releasing.Plan, before ConsoleSnapshot) (ConsoleReceipt, error) {
	if e == nil || e.Deployment == nil || !e.apply.CompareAndSwap(false, true) {
		return ConsoleReceipt{}, ErrTransport
	}
	t := e.Deployment.target
	now := time.Now()
	if p.Version != releasing.PlanVersion || p.TargetSHA256 != t.Digest() || p.PreviousImage != t.BaselineImage || before.Image != p.PreviousImage || !consoleImage(p.Image) || p.CompiledAt <= 0 || p.CompiledAt > now.UnixMilli() || p.ExpiresAt-now.UnixMilli() < releasing.MinimumEffectBudget.Milliseconds() || p.ExpiresAt-p.CompiledAt > releasing.PlanMaxAge.Milliseconds() || p.ExpiresAt > t.ExpiresAt {
		return ConsoleReceipt{}, ErrTransport
	}
	ctx, cancel := context.WithDeadline(ctx, time.UnixMilli(p.ExpiresAt))
	defer cancel()
	return e.patch(ctx, before, p.Image, false)
}

// Rollback requires the original acknowledged advance, current exact generation
// and candidate image, but a freshly observed resourceVersion. Status updates
// may change resourceVersion; a later rollout may never be overwritten. The
// journal gate supplies separate bounded containment authority, even if the
// advance policy was frozen. Ambiguous original PATCH has no receipt to use.
func (e *ConsoleEffects) Rollback(ctx context.Context, original ConsoleReceipt, current ConsoleSnapshot) (ConsoleReceipt, error) {
	if e == nil || e.Deployment == nil || !e.rollback.CompareAndSwap(false, true) {
		return ConsoleReceipt{}, ErrTransport
	}
	t := e.Deployment.target
	expected, err := ConsolePatch(t, original.Before, original.After.Image)
	if err != nil || original.Rollback || authority.Digest(expected) != original.RequestSHA256 || original.Before.Image != t.BaselineImage || !consoleImage(original.After.Image) || original.After.DeploymentUID != t.DeploymentUID || original.After.TargetSHA256 != t.Digest() || original.After.Generation != original.Before.Generation+1 || original.After.ObservedAt < original.Before.ObservedAt || current.DeploymentUID != original.After.DeploymentUID || current.TargetSHA256 != original.After.TargetSHA256 || current.Generation != original.After.Generation || current.Image != original.After.Image {
		return ConsoleReceipt{}, ErrTransport
	}
	return e.patch(ctx, current, t.BaselineImage, true)
}
