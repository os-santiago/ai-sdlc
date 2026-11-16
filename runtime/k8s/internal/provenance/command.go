package provenance

import (
	"encoding/json"
	"regexp"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildrecord"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

const CommandVersion = "ai-sdlc.verify-built-command/v1"
const CommandMaxAge = 2 * time.Minute

var buildIDPattern = regexp.MustCompile(`^build-[a-f0-9]{64}$`)

// Configuration is installation input; evidence and issue text cannot choose it.
// Build holds current issue/policy/profile fences, not renewed build authority.
type Configuration struct {
	Build building.Configuration
	Trust Trust
}

func (cfg Configuration) Validate(now time.Time) error {
	if cfg.Build.Validate() != nil || cfg.Build.Inner.Freeze || cfg.Build.Outer.Freeze || cfg.Trust.Validate(now) != nil || cfg.Trust.Repository != cfg.Build.Outer.Repository || cfg.Trust.Environment != cfg.Build.Outer.Environment {
		return ErrProvenance
	}
	return nil
}

// Command authorizes one bounded read/verification attempt for existing history.
// It contains no artifact choice, manifest, credentials or publication authority.
type Command struct {
	Version             string `json:"version"`
	BuildID             string `json:"buildID"`
	BuildRecordSHA256   string `json:"buildRecordSHA256"`
	TrustSHA256         string `json:"trustSHA256"`
	Repository          string `json:"repository"`
	Environment         string `json:"environment"`
	ProfileSHA256       string `json:"profileSHA256"`
	IntakeConfigSHA256  string `json:"intakeConfigSHA256"`
	InnerPolicyRevision string `json:"innerPolicyRevision"`
	OuterPolicyRevision string `json:"outerPolicyRevision"`
	IssuedAt            int64  `json:"issuedAt"`
	ExpiresAt           int64  `json:"expiresAt"`
}

func (c Command) ID() string { raw, _ := json.Marshal(c); return "verify-" + authority.Digest(raw) }

// NewCommand requires the caller to obtain r from its authenticated journal
// reader and recheck current issue eligibility in the dispatch transaction.
func NewCommand(cfg Configuration, r buildrecord.Record, now time.Time) (Command, error) {
	if cfg.Validate(now) != nil {
		return Command{}, ErrProvenance
	}
	expires := now.Add(CommandMaxAge).UnixMilli()
	if expires > cfg.Trust.ExpiresAt {
		expires = cfg.Trust.ExpiresAt
	}
	if expires-now.UnixMilli() < int64((90*time.Second)/time.Millisecond) {
		return Command{}, ErrProvenance
	}
	c := Command{Version: CommandVersion, BuildID: r.Command.ID(), BuildRecordSHA256: r.Digest(), TrustSHA256: cfg.Trust.Digest(), Repository: cfg.Build.Outer.Repository, Environment: cfg.Build.Outer.Environment, ProfileSHA256: cfg.Build.ProfileDigest(), IntakeConfigSHA256: cfg.Build.IntakeConfigSHA256, InnerPolicyRevision: cfg.Build.Inner.Revision(), OuterPolicyRevision: cfg.Build.Outer.Revision(), IssuedAt: now.UnixMilli(), ExpiresAt: expires}
	if c.BindRecord(cfg, r, now) != nil {
		return Command{}, ErrProvenance
	}
	return c, nil
}
func (c Command) valid(cfg Configuration, now time.Time) error {
	if cfg.Validate(now) != nil || c.Version != CommandVersion || !buildIDPattern.MatchString(c.BuildID) || c.Repository != cfg.Build.Outer.Repository || c.Environment != cfg.Build.Outer.Environment || c.TrustSHA256 != cfg.Trust.Digest() || c.ProfileSHA256 != cfg.Build.ProfileDigest() || c.IntakeConfigSHA256 != cfg.Build.IntakeConfigSHA256 || c.InnerPolicyRevision != cfg.Build.Inner.Revision() || c.OuterPolicyRevision != cfg.Build.Outer.Revision() {
		return ErrProvenance
	}
	for _, h := range []string{c.BuildRecordSHA256, c.TrustSHA256, c.ProfileSHA256, c.IntakeConfigSHA256, c.InnerPolicyRevision, c.OuterPolicyRevision} {
		if !workspacebundle.ValidDigest(h) {
			return ErrProvenance
		}
	}
	issued, expires := time.UnixMilli(c.IssuedAt), time.UnixMilli(c.ExpiresAt)
	if c.IssuedAt <= 0 || issued.After(now.Add(5*time.Second)) || !expires.After(now) || !expires.After(issued) || expires.Sub(issued) > CommandMaxAge || c.ExpiresAt > cfg.Trust.ExpiresAt {
		return ErrProvenance
	}
	return nil
}
func VerifyCommand(cfg Configuration, signed authority.Receipt, raw []byte, now time.Time) (Command, error) {
	var c Command
	if len(raw) == 0 || len(raw) > 8192 || strictObject(raw, &c) != nil || c.valid(cfg, now) != nil || signed.ID != c.ID() || cfg.Build.Outer.VerifyReceipt(signed, "verifier", "verify-built", raw) != nil {
		return Command{}, ErrProvenance
	}
	// Every field must be explicit. Case aliases/unknown fields are rejected above.
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	expected, _ := json.Marshal(c)
	var canonical map[string]json.RawMessage
	_ = json.Unmarshal(expected, &canonical)
	if len(fields) != len(canonical) {
		return Command{}, ErrProvenance
	}
	return c, nil
}
func (c Command) BindRecord(cfg Configuration, r buildrecord.Record, now time.Time) error {
	if c.valid(cfg, now) != nil || !cfg.Trust.matches(r) || c.BuildID != r.Command.ID() || c.BuildRecordSHA256 != r.Digest() || r.Command.InnerPolicyRevision != c.InnerPolicyRevision || r.Command.OuterPolicyRevision != c.OuterPolicyRevision || r.Command.ProfileSHA256 != c.ProfileSHA256 || r.Command.IntakeConfigSHA256 != c.IntakeConfigSHA256 {
		return ErrProvenance
	}
	return nil
}

// BindProof checks the verifier's result at durable completion. Only the verifier
// identity may write that result; structural validation cannot replace its role.
func (c Command) BindProof(cfg Configuration, r buildrecord.Record, p Proof, now time.Time) error {
	raw, err := r.Context.Encode()
	if err != nil || c.BindRecord(cfg, r, now) != nil || p.ReleaseAuthorized || p.Version != PolicyVersion || p.Scope != LocalScope || p.BuildRecordSHA256 != c.BuildRecordSHA256 || p.RetainedContextSHA256 != authority.Digest(raw) || p.TrustSHA256 != c.TrustSHA256 || p.VerifierImage != cfg.Trust.VerifierImage || p.VerifierVersion != VerifierVersion || p.ImageDigest != r.Result.ImageDigest || p.BuildObservationSHA256 != r.Build.Digest() || p.PublishObservationSHA256 != r.Publish.Digest() || p.Transparency != LocalTransparency {
		return ErrProvenance
	}
	for _, d := range []string{p.SignatureManifestDigest, p.SignatureBlobDigest, p.AttestationManifestDigest, p.AttestationBlobDigest} {
		if !sha256RE.MatchString(d) {
			return ErrProvenance
		}
	}
	return nil
}
