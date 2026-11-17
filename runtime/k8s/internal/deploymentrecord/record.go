// Package deploymentrecord holds historical facts, never new effect authority.
package deploymentrecord

import (
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildrecord"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/deployment"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/promotionrecord"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/provenancerecord"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	"time"
)

// Field order retains the original store.DeploymentRecord digest. Only the
// role-checked store reader authenticates the claim, intent, send and upstream.
type Record struct {
	Command       deployment.Command
	Configuration releasing.Configuration
	Plan          releasing.Plan
	Receipt       releasing.Receipt
	Health        releasing.Health
	Outcome       string
	RecordedAt    time.Time
}

func (r Record) Digest() string { raw, _ := json.Marshal(r); return authority.Digest(raw) }
func (r Record) Validate(p promotionrecord.Record, v provenancerecord.Record, b buildrecord.Record) error {
	if r.Outcome != "deployed" || r.Receipt.Rollback || r.Plan.Digest() != p.Plan.Digest() || r.Command.BindRecords(r.Configuration, p, v, b, time.UnixMilli(r.Command.IssuedAt)) != nil || r.RecordedAt.Before(p.RecordedAt) || r.RecordedAt.Before(time.UnixMilli(r.Command.IssuedAt)) || !r.RecordedAt.Before(time.UnixMilli(r.Command.ExpiresAt)) || deployment.ValidateHealth(r.Configuration.Target, r.Receipt, r.Health, r.RecordedAt) != nil || deployment.ValidateBrowserHealth(r.Configuration.Browser, r.Configuration.Target, r.Receipt, r.Health) != nil {
		return deployment.ErrDeployment
	}
	raw, e := deployment.CompilePatch(r.Configuration.Target, r.Receipt.Before, r.Plan.Image, time.UnixMilli(r.Receipt.Before.ObservedAt))
	if e != nil || deployment.ValidateReceipt(r.Configuration.Target, r.Plan, r.Receipt.Before, authority.Digest(raw), false, r.Receipt, time.UnixMilli(r.Receipt.Before.ObservedAt), r.RecordedAt) != nil {
		return deployment.ErrDeployment
	}
	return nil
}
