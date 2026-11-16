// Package releasecompletion authorizes only final recording of an authenticated
// successful deployment. It never renews deployment or rollback authority.
package releasecompletion

import (
	"encoding/json"
	"errors"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildrecord"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/deployment"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/deploymentrecord"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/promotionrecord"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/provenancerecord"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
	"regexp"
	"time"
)

const Version = "ai-sdlc.record-deployment-command/v1"
const MaxAge = 5 * time.Minute

var ErrCompletion = errors.New("terminal release authority denied")
var deploymentID = regexp.MustCompile(`^deploy-[a-f0-9]{64}$`)

type Command struct {
	Version                string `json:"version"`
	DeploymentID           string `json:"deploymentID"`
	DeploymentRecordSHA256 string `json:"deploymentRecordSHA256"`
	ConfigurationSHA256    string `json:"configurationSHA256"`
	IssuedAt               int64  `json:"issuedAt"`
	ExpiresAt              int64  `json:"expiresAt"`
}

func (c Command) ID() string { raw, _ := json.Marshal(c); return "complete-" + authority.Digest(raw) }
func NewCommand(cfg releasing.Configuration, r deploymentrecord.Record, p promotionrecord.Record, v provenancerecord.Record, b buildrecord.Record, now time.Time) (Command, error) {
	issued := now.Truncate(time.Millisecond)
	if issued.Before(now) {
		issued = issued.Add(time.Millisecond)
	}
	c := Command{Version, r.Command.ID(), r.Digest(), deployment.ConfigurationDigest(cfg), issued.UnixMilli(), min(now.Add(MaxAge).UnixMilli(), cfg.Target.ExpiresAt, cfg.Provenance.Trust.ExpiresAt)}
	if c.BindRecords(cfg, r, p, v, b, now) != nil {
		return Command{}, ErrCompletion
	}
	return c, nil
}
func (c Command) valid(cfg releasing.Configuration, now time.Time) error {
	if cfg.Validate(now) != nil || c.Version != Version || !deploymentID.MatchString(c.DeploymentID) || !workspacebundle.ValidDigest(c.DeploymentRecordSHA256) || c.ConfigurationSHA256 != deployment.ConfigurationDigest(cfg) || c.IssuedAt <= 0 || c.IssuedAt > now.Add(5*time.Second).UnixMilli() || c.ExpiresAt <= now.UnixMilli() || c.ExpiresAt <= c.IssuedAt || c.ExpiresAt-c.IssuedAt > MaxAge.Milliseconds() || c.ExpiresAt > cfg.Target.ExpiresAt || c.ExpiresAt > cfg.Provenance.Trust.ExpiresAt {
		return ErrCompletion
	}
	return nil
}
func (c Command) BindRecords(cfg releasing.Configuration, r deploymentrecord.Record, p promotionrecord.Record, v provenancerecord.Record, b buildrecord.Record, now time.Time) error {
	if c.valid(cfg, now) != nil || r.Validate(p, v, b) != nil || r.RecordedAt.After(now) || r.RecordedAt.After(time.UnixMilli(c.IssuedAt)) || c.DeploymentID != r.Command.ID() || c.DeploymentRecordSHA256 != r.Digest() || c.ConfigurationSHA256 != deployment.ConfigurationDigest(r.Configuration) {
		return ErrCompletion
	}
	return nil
}
func VerifyCommand(cfg releasing.Configuration, signed authority.Receipt, raw []byte, now time.Time) (Command, error) {
	var c Command
	if len(raw) == 0 || len(raw) > 4096 || authority.DecodeStrict(raw, &c) != nil || c.valid(cfg, now) != nil || signed.ID != c.ID() || cfg.Provenance.Build.Outer.VerifyReceipt(signed, "release-manager", "record-deployment", raw) != nil {
		return Command{}, ErrCompletion
	}
	var fields map[string]json.RawMessage
	canonical, _ := json.Marshal(c)
	var expected map[string]json.RawMessage
	_ = json.Unmarshal(canonical, &expected)
	if json.Unmarshal(raw, &fields) != nil || len(fields) != len(expected) {
		return Command{}, ErrCompletion
	}
	for key := range fields {
		if _, ok := expected[key]; !ok {
			return Command{}, ErrCompletion
		}
	}
	return c, nil
}
