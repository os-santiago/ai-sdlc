package releasecompletion

import (
	"encoding/json"
	"fmt"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildrecord"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/deploymentrecord"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/promotionrecord"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/provenancerecord"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	"strconv"
	"time"
)

// Plan is derived exclusively from the authenticated completion and its exact
// historical source/artifact. No model-authored tag, title, notes or URL enters it.
type Plan struct {
	Version                string `json:"version"`
	CommandID              string `json:"commandID"`
	Repository             string `json:"repository"`
	IssueNumber            int    `json:"issueNumber"`
	Generation             int64  `json:"generation"`
	IssueRevision          string `json:"issueRevision"`
	SourceSHA              string `json:"sourceSHA"`
	TreeSHA                string `json:"treeSHA"`
	Image                  string `json:"image"`
	TargetSHA256           string `json:"targetSHA256"`
	DeploymentRecordSHA256 string `json:"deploymentRecordSHA256"`
	ProvenanceRecordSHA256 string `json:"provenanceRecordSHA256"`
	BuildRecordSHA256      string `json:"buildRecordSHA256"`
	Tag                    string `json:"tag"`
	Title                  string `json:"title"`
	Body                   string `json:"body"`
	ExpiresAt              int64  `json:"expiresAt"`
}

func (p Plan) Digest() string { raw, _ := json.Marshal(p); return authority.Digest(raw) }
func CompilePlan(cfg releasing.Configuration, c Command, r deploymentrecord.Record, p promotionrecord.Record, v provenancerecord.Record, b buildrecord.Record, now time.Time) (Plan, error) {
	if c.BindRecords(cfg, r, p, v, b, now) != nil || b.Command.Repository != "os-santiago/saferus" {
		return Plan{}, ErrCompletion
	}
	plan := Plan{Version: "ai-sdlc.terminal-release-plan/v1", CommandID: c.ID(), Repository: b.Command.Repository, IssueNumber: b.Command.IssueNumber, Generation: b.Command.Generation, IssueRevision: b.Command.IssueRevision, SourceSHA: b.Command.SourceSHA, TreeSHA: b.Command.TreeSHA, Image: r.Plan.Image, TargetSHA256: cfg.Target.Digest(), DeploymentRecordSHA256: r.Digest(), ProvenanceRecordSHA256: v.Digest(), BuildRecordSHA256: b.Digest(), Tag: "ai-sdlc/" + c.ID(), Title: "AI-SDLC kind test production: issue #" + strconv.Itoa(b.Command.IssueNumber), ExpiresAt: c.ExpiresAt}
	plan.Body = plan.BodyText()
	return plan, nil
}
func (p Plan) BindRecords(cfg releasing.Configuration, c Command, r deploymentrecord.Record, promotion promotionrecord.Record, v provenancerecord.Record, b buildrecord.Record, now time.Time) error {
	expected, e := CompilePlan(cfg, c, r, promotion, v, b, now)
	if e != nil || p != expected {
		return ErrCompletion
	}
	return nil
}

func (p Plan) BodyText() string {
	return fmt.Sprintf("Scope: kind-test-production\n\nIssue: %s#%d (generation %d)\n\nSource: `%s`\n\nDeployed image: `%s`\n\nDeployment record: `%s`\n\nProvenance record: `%s`\n\nBuild record: `%s`\n\nTarget: `%s`\n\n<!-- ai-sdlc-release:v1 %s -->\n", p.Repository, p.IssueNumber, p.Generation, p.SourceSHA, p.Image, p.DeploymentRecordSHA256, p.ProvenanceRecordSHA256, p.BuildRecordSHA256, p.TargetSHA256, p.CommandID)
}
