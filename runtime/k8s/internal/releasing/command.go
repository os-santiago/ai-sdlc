package releasing

import (
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/browsercheck"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildrecord"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/provenance"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/provenancerecord"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
	"regexp"
	"time"
)

const CommandVersion = "ai-sdlc.promote-artifact-command/v1"
const CommandMaxAge = 2 * time.Minute

var verificationID = regexp.MustCompile(`^verify-[a-f0-9]{64}$`)

type Configuration struct {
	Provenance provenance.Configuration
	Target     Target
	Browser    browsercheck.Profile
}

func (c Configuration) Validate(now time.Time) error {
	if c.Provenance.Validate(now) != nil || c.Target.Validate(now) != nil || c.Browser.Validate() != nil || c.Browser.ClusterUID != c.Target.ClusterUID || c.Browser.CASHA256 != authority.Digest([]byte(c.Target.TLSCA)) || c.Target.ClusterUID != c.Provenance.Trust.ClusterUID || c.Target.Repository != c.Provenance.Build.Outer.Repository || c.Target.Environment != c.Provenance.Build.Outer.Environment {
		return ErrRelease
	}
	return nil
}

// Command requests one promotion decision. It carries no arbitrary workload,
// executable input, destination URL, or credentials. Deployment needs its own
// journal and separate deploy-release authority after this bounded decision.
type Command struct {
	Version                string `json:"version"`
	ProvenanceID           string `json:"provenanceID"`
	ProvenanceRecordSHA256 string `json:"provenanceRecordSHA256"`
	BuildRecordSHA256      string `json:"buildRecordSHA256"`
	TargetSHA256           string `json:"targetSHA256"`
	TrustSHA256            string `json:"trustSHA256"`
	Repository             string `json:"repository"`
	Environment            string `json:"environment"`
	IssueNumber            int    `json:"issueNumber"`
	IssueRevision          string `json:"issueRevision"`
	Generation             int64  `json:"generation"`
	SourceSHA              string `json:"sourceSHA"`
	ImageDigest            string `json:"imageDigest"`
	ProfileSHA256          string `json:"profileSHA256"`
	IntakeConfigSHA256     string `json:"intakeConfigSHA256"`
	InnerPolicyRevision    string `json:"innerPolicyRevision"`
	OuterPolicyRevision    string `json:"outerPolicyRevision"`
	IssuedAt               int64  `json:"issuedAt"`
	ExpiresAt              int64  `json:"expiresAt"`
}

func (c Command) ID() string { raw, _ := json.Marshal(c); return "promote-" + authority.Digest(raw) }

// Callers MUST obtain both records from authenticated journal readers and
// recheck issue eligibility within the dispatch transaction. This pure function
// cannot establish either property. Expired historical build/verify commands
// remain history; this command grants a separately bounded promotion attempt.
func NewCommand(cfg Configuration, r provenancerecord.Record, b buildrecord.Record, now time.Time) (Command, error) {
	if cfg.Validate(now) != nil {
		return Command{}, ErrRelease
	}
	expires := now.Add(CommandMaxAge).UnixMilli()
	for _, limit := range []int64{cfg.Target.ExpiresAt, cfg.Provenance.Trust.ExpiresAt} {
		if expires > limit {
			expires = limit
		}
	}
	if expires-now.UnixMilli() < int64((90*time.Second)/time.Millisecond) {
		return Command{}, ErrRelease
	}
	c := Command{Version: CommandVersion, ProvenanceID: r.Command.ID(), ProvenanceRecordSHA256: r.Digest(), BuildRecordSHA256: b.Digest(), TargetSHA256: cfg.Target.Digest(), TrustSHA256: cfg.Provenance.Trust.Digest(), Repository: cfg.Target.Repository, Environment: cfg.Target.Environment, IssueNumber: b.Command.IssueNumber, IssueRevision: b.Command.IssueRevision, Generation: b.Command.Generation, SourceSHA: b.Command.SourceSHA, ImageDigest: r.Proof.ImageDigest, ProfileSHA256: cfg.Provenance.Build.ProfileDigest(), IntakeConfigSHA256: cfg.Provenance.Build.IntakeConfigSHA256, InnerPolicyRevision: cfg.Provenance.Build.Inner.Revision(), OuterPolicyRevision: cfg.Provenance.Build.Outer.Revision(), IssuedAt: now.UnixMilli(), ExpiresAt: expires}
	if c.BindRecords(cfg, r, b, now) != nil {
		return Command{}, ErrRelease
	}
	return c, nil
}
func (c Command) valid(cfg Configuration, now time.Time) error {
	if cfg.Validate(now) != nil || c.Version != CommandVersion || !verificationID.MatchString(c.ProvenanceID) || c.Repository != cfg.Target.Repository || c.Environment != cfg.Target.Environment || c.TargetSHA256 != cfg.Target.Digest() || c.TrustSHA256 != cfg.Provenance.Trust.Digest() || c.ProfileSHA256 != cfg.Provenance.Build.ProfileDigest() || c.IntakeConfigSHA256 != cfg.Provenance.Build.IntakeConfigSHA256 || c.InnerPolicyRevision != cfg.Provenance.Build.Inner.Revision() || c.OuterPolicyRevision != cfg.Provenance.Build.Outer.Revision() || c.IssueNumber < 1 || c.IssueNumber > 2147483647 || c.Generation < 1 || !workspacebundle.ValidSourceSHA(c.SourceSHA) || !imageDigest.MatchString(c.ImageDigest) {
		return ErrRelease
	}
	for _, d := range []string{c.ProvenanceRecordSHA256, c.BuildRecordSHA256, c.TargetSHA256, c.TrustSHA256, c.IssueRevision, c.ProfileSHA256, c.IntakeConfigSHA256, c.InnerPolicyRevision, c.OuterPolicyRevision} {
		if !workspacebundle.ValidDigest(d) {
			return ErrRelease
		}
	}
	issued, expires := time.UnixMilli(c.IssuedAt), time.UnixMilli(c.ExpiresAt)
	if c.IssuedAt <= 0 || issued.After(now.Add(5*time.Second)) || !expires.After(now) || !expires.After(issued) || expires.Sub(issued) > CommandMaxAge || c.ExpiresAt > cfg.Target.ExpiresAt || c.ExpiresAt > cfg.Provenance.Trust.ExpiresAt {
		return ErrRelease
	}
	return nil
}
func VerifyCommand(cfg Configuration, signed authority.Receipt, raw []byte, now time.Time) (Command, error) {
	var c Command
	if len(raw) == 0 || len(raw) > 16384 || authority.DecodeStrict(raw, &c) != nil || !exactKeys(raw, c) || c.valid(cfg, now) != nil || signed.ID != c.ID() || cfg.Provenance.Build.Outer.VerifyReceipt(signed, "release-manager", "promote-artifact", raw) != nil {
		return Command{}, ErrRelease
	}
	return c, nil
}
func exactKeys(raw []byte, c Command) bool {
	var actual, expected map[string]json.RawMessage
	canonical, _ := json.Marshal(c)
	if json.Unmarshal(raw, &actual) != nil || json.Unmarshal(canonical, &expected) != nil || len(actual) != len(expected) {
		return false
	}
	for k := range actual {
		if _, ok := expected[k]; !ok {
			return false
		}
	}
	return true
}
func (c Command) BindRecords(cfg Configuration, r provenancerecord.Record, b buildrecord.Record, now time.Time) error {
	if c.valid(cfg, now) != nil || r.Validate(b) != nil || r.RecordedAt.After(time.UnixMilli(c.IssuedAt)) || r.RecordedAt.After(now) || c.ProvenanceID != r.Command.ID() || c.ProvenanceRecordSHA256 != r.Digest() || c.BuildRecordSHA256 != b.Digest() || c.TrustSHA256 != r.Command.TrustSHA256 || c.ProfileSHA256 != r.Command.ProfileSHA256 || c.IntakeConfigSHA256 != r.Command.IntakeConfigSHA256 || c.InnerPolicyRevision != r.Command.InnerPolicyRevision || c.OuterPolicyRevision != r.Command.OuterPolicyRevision || c.IssueNumber != b.Command.IssueNumber || c.IssueRevision != b.Command.IssueRevision || c.Generation != b.Command.Generation || c.SourceSHA != b.Command.SourceSHA || c.ImageDigest != r.Proof.ImageDigest {
		return ErrRelease
	}
	return nil
}
