// Package deployment binds one finite deployer attempt to authenticated history.
package deployment

import (
	"encoding/json"
	"errors"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildrecord"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/promotionrecord"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/provenancerecord"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
	"regexp"
	"time"
)

const Version = "ai-sdlc.deploy-release-command/v1"

var ErrDeployment = errors.New("deployment authority rejected")
var promotionID = regexp.MustCompile(`^promote-[a-f0-9]{64}$`)

// Configuration digest covers current policies, trust, profile and exact target.
// The immutable promotion digest transitively binds issue, generation, source,
// build, verified artifact and plan. No user URL or executable input is accepted.
type Command struct {
	Version               string `json:"version"`
	PromotionID           string `json:"promotionID"`
	PromotionRecordSHA256 string `json:"promotionRecordSHA256"`
	PlanSHA256            string `json:"planSHA256"`
	ConfigurationSHA256   string `json:"configurationSHA256"`
	IssuedAt              int64  `json:"issuedAt"`
	ExpiresAt             int64  `json:"expiresAt"`
}

func ConfigurationDigest(c releasing.Configuration) string {
	b, _ := json.Marshal(c)
	return authority.Digest(b)
}
func (c Command) ID() string { b, _ := json.Marshal(c); return "deploy-" + authority.Digest(b) }

// Callers must authenticate every record and current issue eligibility. A pure
// value, including one passing this function, is not evidence of its origin.
func NewCommand(cfg releasing.Configuration, r promotionrecord.Record, p provenancerecord.Record, b buildrecord.Record, now time.Time) (Command, error) {
	c := Command{Version, r.Command.ID(), r.Digest(), r.Plan.Digest(), ConfigurationDigest(cfg), now.UnixMilli(), r.Plan.ExpiresAt}
	if c.BindRecords(cfg, r, p, b, now) != nil || c.ExpiresAt-now.UnixMilli() < releasing.MinimumEffectBudget.Milliseconds() {
		return Command{}, ErrDeployment
	}
	return c, nil
}
func (c Command) valid(cfg releasing.Configuration, now time.Time) error {
	if cfg.Validate(now) != nil || c.Version != Version || !promotionID.MatchString(c.PromotionID) || c.ConfigurationSHA256 != ConfigurationDigest(cfg) {
		return ErrDeployment
	}
	for _, d := range []string{c.PromotionRecordSHA256, c.PlanSHA256, c.ConfigurationSHA256} {
		if !workspacebundle.ValidDigest(d) {
			return ErrDeployment
		}
	}
	if c.IssuedAt <= 0 || c.IssuedAt > now.Add(5*time.Second).UnixMilli() || c.ExpiresAt <= now.UnixMilli() || c.ExpiresAt <= c.IssuedAt || c.ExpiresAt-c.IssuedAt > releasing.PlanMaxAge.Milliseconds() || c.ExpiresAt > cfg.Target.ExpiresAt || c.ExpiresAt > cfg.Provenance.Trust.ExpiresAt {
		return ErrDeployment
	}
	return nil
}
func (c Command) BindRecords(cfg releasing.Configuration, r promotionrecord.Record, p provenancerecord.Record, b buildrecord.Record, now time.Time) error {
	if c.valid(cfg, now) != nil || r.Validate(p, b) != nil || r.RecordedAt.After(time.UnixMilli(c.IssuedAt)) || r.RecordedAt.After(now) || c.PromotionID != r.Command.ID() || c.PromotionRecordSHA256 != r.Digest() || c.PlanSHA256 != r.Plan.Digest() || c.ConfigurationSHA256 != ConfigurationDigest(r.Configuration) || c.ExpiresAt != r.Plan.ExpiresAt {
		return ErrDeployment
	}
	return nil
}
func VerifyCommand(cfg releasing.Configuration, signed authority.Receipt, raw []byte, now time.Time) (Command, error) {
	var c Command
	if len(raw) == 0 || len(raw) > 4096 || authority.DecodeStrict(raw, &c) != nil || !exactKeys(raw, c) || c.valid(cfg, now) != nil || signed.ID != c.ID() || cfg.Provenance.Build.Outer.VerifyReceipt(signed, "deployer", "deploy-release", raw) != nil {
		return Command{}, ErrDeployment
	}
	return c, nil
}
func exactKeys(raw []byte, c Command) bool {
	var a, b map[string]json.RawMessage
	canonical, _ := json.Marshal(c)
	if json.Unmarshal(raw, &a) != nil || json.Unmarshal(canonical, &b) != nil || len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}
