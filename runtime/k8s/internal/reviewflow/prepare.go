// Package reviewflow is the independent review controller, separate from code
// generation. Only already verified, signed candidates may enter its queue.
package reviewflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/repositorytests"
	"time"
	"unicode/utf8"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/candidate"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelbroker"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/normalizedevent"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/sourcefetch"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verification"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verificationflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

var ErrReview = errors.New("independent review rejected")

type Configuration struct {
	Namespace, WorkerImage, MailboxImage string
	Profile, ADev                        []byte
	Model                                modelbroker.ProviderConfig
	// RequireGatewayProfile refuses reviews unless the gateway's benchmarked
	// timeout fits the reviewer call window. Installation-only; not digested.
	RequireGatewayProfile bool `json:"-"`
}
type Loader func(context.Context) (Configuration, error)

func (c Configuration) Digest() string {
	c.Model.BearerToken = ""
	// Installation-only, like the token: the verifier and the publisher must
	// compute the same digest whatever broker timeout each process was given.
	c.Model.TimeoutSeconds = 0
	raw, _ := json.Marshal(c)
	return authority.Digest(raw)
}
func (c Configuration) Validate() error {
	p, e := executor.ParseReviewProfile(c.Profile, authority.Digest(c.Profile))
	if e != nil || p.MaxSteps < 2 || len(c.ADev) < 1 || len(c.ADev) > 65536 || !utf8.Valid(c.ADev) || authority.Digest(c.ADev) != p.ADevSHA256 || c.Namespace != "ai-sdlc-reviewers" {
		return ErrReview
	}
	if _, e = modelbroker.NewProvider(c.Model); e != nil {
		return ErrReview
	}
	return nil
}
func loadConfiguration(ctx context.Context, load Loader) (Configuration, error) {
	if load == nil {
		return Configuration{}, ErrReview
	}
	c, e := load(ctx)
	if e != nil {
		return Configuration{}, store.ErrObservationUnavailable
	}
	c.Profile = append([]byte(nil), c.Profile...)
	c.ADev = append([]byte(nil), c.ADev...)
	if c.Validate() != nil {
		return Configuration{}, ErrReview
	}
	return c, nil
}

type prepared struct {
	run     store.ReviewRun
	command verification.Command
	binding podtransport.Binding
	source  []byte
	tree    string
	config  Configuration
}
type sourceLoader func(context.Context, string, string) (workspacebundle.Bundle, error)

func fetchSource(ctx context.Context, repository, sha string) (workspacebundle.Bundle, error) {
	c, e := sourcefetch.New(repository, "")
	if e != nil {
		return workspacebundle.Bundle{}, e
	}
	return c.Fetch(ctx, sha)
}

// Prepare reconstructs the exact candidate before queueing review; it does not
// execute source code or reserve a model call. This bounded fetch is replayable.
func prepare(ctx context.Context, db *store.DB, policy verificationflow.PolicyLoader, load Loader, id string, fetch sourceLoader) (prepared, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	config, e := loadConfiguration(ctx, load)
	if e != nil {
		return prepared{}, e
	}
	command, record, snapshot, e := verificationflow.CurrentProof(ctx, db, policy, id)
	if e != nil {
		return prepared{}, e
	}
	raw, e := db.ReadBehaviorProof(ctx, id)
	if e != nil {
		return prepared{}, e
	}
	var behavior verificationflow.BehaviorProof
	if authority.DecodeStrict(raw, &behavior) != nil || !verificationflow.ValidBehaviorProof(behavior, command) {
		return prepared{}, ErrReview
	}
	source, e := fetch(ctx, command.Repository, command.SourceSHA)
	if e != nil {
		return prepared{}, ErrReview
	}
	if _, e = verification.Prepare(command.Repository, command.Recipe, snapshot.Issue.Body, source, record.Delta); e != nil {
		return prepared{}, ErrReview
	}
	delta, e := candidate.Decode(record.Delta)
	if e != nil {
		return prepared{}, ErrReview
	}
	tree, e := candidate.Apply(source, delta)
	if e != nil {
		return prepared{}, ErrReview
	}
	treeDigest, e := tree.TreeDigest()
	if e != nil || treeDigest != command.CandidateTreeSHA256 {
		return prepared{}, ErrReview
	}
	sourceRaw, e := workspacebundle.Encode(tree)
	if e != nil {
		return prepared{}, ErrReview
	}
	scope := []string{}
	for _, change := range delta.Changes {
		scope = append(scope, change.Path)
	}
	scopeRaw, _ := json.Marshal(scope)
	reviewBody := snapshot.Issue.Body + "\n\nVerified candidate file paths (read these changes and their tests):\n" + string(scopeRaw)
	if summary := verificationSummary(behavior); summary != "" {
		reviewBody += "\n\n" + summary
	}
	a := executor.Assignment{Version: executor.AssignmentVersion, WorkItemID: command.WorkItemID, Attempt: 1, Repository: command.Repository, SourceSHA: command.SourceSHA, TreeSHA256: treeDigest, ProfileSHA256: authority.Digest(config.Profile), AuthorityRevision: command.AuthorityRevision, IssueNumbers: []int{command.IssueNumber}, Title: snapshot.Issue.Title, Body: reviewBody, AcceptanceCriteria: normalizedevent.ExtractAcceptanceCriteria(snapshot.Issue.Body), ExpiresAt: command.ExpiresAt}
	assignment, _ := json.Marshal(a)
	if _, e = executor.ParseAssignment(assignment, authority.Digest(assignment), time.Now()); e != nil {
		return prepared{}, ErrReview
	}
	current, _, _, e := verificationflow.CurrentProof(ctx, db, policy, id)
	if e != nil || current != command {
		return prepared{}, ErrReview
	}
	latest, e := loadConfiguration(ctx, load)
	if e != nil || latest.Digest() != config.Digest() {
		return prepared{}, ErrReview
	}
	proof, e := db.ReadBehaviorProof(ctx, id)
	if e != nil || !bytes.Equal(raw, proof) {
		return prepared{}, ErrReview
	}
	run := store.ReviewRun{ID: store.ReviewID(id, authority.Digest(raw), a.ProfileSHA256, config.Digest(), authority.Digest(assignment)), VerificationID: id, CandidateID: command.CandidateID, WorkItem: command.WorkItemID, BehaviorDigest: authority.Digest(raw), ProfileDigest: a.ProfileSHA256, ConfigurationDigest: config.Digest(), AssignmentDigest: authority.Digest(assignment), ExpiresAt: time.UnixMilli(command.ExpiresAt)}
	return prepared{run: run, command: command, binding: podtransport.Binding{Namespace: config.Namespace, WorkerImage: config.WorkerImage, MailboxImage: config.MailboxImage, Assignment: assignment, Profile: config.Profile, ADev: config.ADev}, source: sourceRaw, tree: treeDigest, config: config}, nil
}

type currentAuthority struct {
	db       *store.DB
	policy   verificationflow.PolicyLoader
	load     Loader
	expected prepared
}

func (a currentAuthority) AuthorizeReview(ctx context.Context, run store.ReviewRun, claim store.ReviewClaim) (time.Time, error) {
	if !sameRun(run, a.expected.run) || claim.ID != run.ID {
		return time.Time{}, ErrReview
	}
	c, _, _, e := verificationflow.CurrentProof(ctx, a.db, a.policy, run.VerificationID)
	if e != nil || c != a.expected.command {
		return time.Time{}, ErrReview
	}
	config, e := loadConfiguration(ctx, a.load)
	if e != nil || config.Digest() != run.ConfigurationDigest {
		return time.Time{}, ErrReview
	}
	proof, e := a.db.ReadBehaviorProof(ctx, run.VerificationID)
	if e != nil || authority.Digest(proof) != run.BehaviorDigest {
		return time.Time{}, ErrReview
	}
	if a.db.CheckReviewClaim(ctx, claim) != nil {
		return time.Time{}, ErrReview
	}
	return claim.Until, nil
}

// verificationSummary tells the reviewer what the repository's own tests
// showed (v2 proofs only). A weak decision needs the reviewer's judgement.
func verificationSummary(b verificationflow.BehaviorProof) string {
	if b.Version != verificationflow.BehaviorProofV2 {
		return ""
	}
	var d repositorytests.Decision
	if authority.DecodeStrict(b.Decision, &d) != nil {
		return ""
	}
	s := d.Summary
	files, _ := json.Marshal(s.ChangedTestFiles)
	text := fmt.Sprintf("Verification with the repository's own tests: baseline exit %d, candidate tests on the base code exit %d, candidate exit %d over %d runs, %d failing-to-passing tests, %d regressions. Changed test files: %s.",
		s.BaselineExitCode, s.CrossExitCode, s.CandidateExitCode, s.CandidateRuns, s.FailToPass, s.Regressions, files)
	if s.Weak {
		text += " This is a weak result (no per-test report): it shows only that the changed tests fail on the base code. Judge whether they test the requested behaviour, rather than failing for another reason."
	}
	return text
}
