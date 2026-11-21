package verificationflow

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/repositorytests"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/sourcefetch"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verification"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

type ControllerError struct{ Stage string }

func (e ControllerError) Error() string { return "verification stopped at " + e.Stage }

type BehaviorProof struct {
	Version               string               `json:"version"`
	Command               verification.Command `json:"command"`
	ToolchainImage        string               `json:"toolchainImage"`
	BaselineResultSHA256  string               `json:"baselineResultSHA256"`
	CandidateResultSHA256 string               `json:"candidateResultSHA256"`
	ReferenceResultSHA256 string               `json:"referenceResultSHA256,omitempty"`
	// v2 (repository-tests/v1): the cross run's result digest and the
	// reviewable decision with its summary.
	CrossResultSHA256     string            `json:"crossResultSHA256,omitempty"`
	Decision              json.RawMessage   `json:"decision,omitempty"`
	ResourceUIDs          map[string]string `json:"resourceUIDs"`
	BehaviorVerified      bool              `json:"behaviorVerified"`
	PublicationAuthorized bool              `json:"publicationAuthorized"`
}

// Behavior proof versions: v1 for the reference recipes (five resources),
// v2 for repository-tests/v1 (settings and three runs).
const (
	BehaviorProofV1 = "ai-sdlc.behavior-proof/v1"
	BehaviorProofV2 = "ai-sdlc.behavior-proof/v2"
)

// DecisionError is a verified refusal by the repository-tests rules: the
// runs completed and the decision is the reason, not an uncertainty.
type DecisionError struct{ Reason string }

func (e DecisionError) Error() string { return "verification refused: " + e.Reason }

// ValidBehaviorProof checks the shape every consumer requires of a passed
// behavior proof for the command.
func ValidBehaviorProof(b BehaviorProof, c verification.Command) bool {
	if !b.BehaviorVerified || b.PublicationAuthorized || b.Command != c {
		return false
	}
	keys := []string{"config", "baseline", "candidate", "reference", "tests"}
	switch {
	case b.Version == BehaviorProofV1 && c.Recipe != repositorytests.Recipe && len(b.Decision) == 0 && b.CrossResultSHA256 == "":
	case b.Version == BehaviorProofV2 && c.Recipe == repositorytests.Recipe && len(b.Decision) != 0 && b.ReferenceResultSHA256 == "" &&
		workspacebundle.ValidDigest(b.BaselineResultSHA256) && workspacebundle.ValidDigest(b.CrossResultSHA256) &&
		workspacebundle.ValidDigest(b.CandidateResultSHA256) && b.ToolchainImage != "":
		keys = []string{"config", "baseline", "tests", "candidate"}
		var d repositorytests.Decision
		if authority.DecodeStrict(b.Decision, &d) != nil || !d.Passed || d.Reason != "" {
			return false
		}
	default:
		return false
	}
	if len(b.ResourceUIDs) != len(keys) {
		return false
	}
	for _, key := range keys {
		if b.ResourceUIDs[key] == "" {
			return false
		}
	}
	return true
}

// Run consumes only an already signed durable inbox identifier. It never
// accepts arbitrary commands, adopts claims, invokes a model, or publishes code.
func Run(ctx context.Context, db *store.DB, load PolicyLoader, id string, client *podtransport.Client, image string) (proof BehaviorProof, err error) {
	return RunWithMailbox(ctx, db, load, id, client, image, "")
}

// RunWithMailbox also receives the installation's pinned mailbox image, which
// carries the repository-tests runner and transfer helper.
func RunWithMailbox(ctx context.Context, db *store.DB, load PolicyLoader, id string, client *podtransport.Client, image, mailboxImage string) (proof BehaviorProof, err error) {
	if db == nil || load == nil || client == nil {
		return proof, ControllerError{"configuration"}
	}
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	c, record, snapshot, err := CurrentProof(ctx, db, load, id)
	if err != nil {
		return proof, ControllerError{"authority"}
	}
	request, err := db.ReadVerification(ctx, id)
	if err != nil {
		return proof, ControllerError{"request"}
	}
	claim, err := db.ClaimVerification(ctx, id, request.PayloadDigest)
	if err != nil {
		return proof, ControllerError{"claim"}
	}
	ctx, claimCancel := context.WithDeadline(ctx, claim.Until)
	defer claimCancel()
	stage := "authority"
	var resources *podtransport.VerificationResources
	var testRuns map[string]*podtransport.TestsRun // by ledger kind
	configUID := ""
	pods := map[string]string{}
	cleaned := false
	cleanupAttempted := false
	cleanup := func() error {
		cleanupAttempted = true
		cleanupCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		failed := false
		if resources != nil {
			for kind, uid := range pods {
				if resources.DeletePod(cleanupCtx, kind, uid) != nil {
					failed = true
				}
			}
			if configUID != "" && resources.DeleteConfig(cleanupCtx, configUID) != nil {
				failed = true
			}
		} else if testRuns != nil {
			for kind, uid := range pods {
				if testRuns[kind] == nil || testRuns[kind].DeletePod(cleanupCtx, uid) != nil {
					failed = true
				}
			}
			if configUID != "" && testRuns["baseline"].DeleteConfig(cleanupCtx, configUID) != nil {
				failed = true
			}
		} else if configUID != "" || len(pods) != 0 {
			failed = true
		}
		if failed {
			return ControllerError{"cleanup"}
		}
		if db.ConfirmVerificationCleanup(cleanupCtx, claim) != nil {
			return ControllerError{"cleanup"}
		}
		cleaned = true
		return nil
	}
	defer func() {
		if recover() != nil {
			err = ControllerError{stage}
		}
		if !cleaned && !cleanupAttempted {
			if e := cleanup(); e != nil {
				err = e
			}
		}
		if err != nil {
			proof = BehaviorProof{}
			stopCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
			defer stop()
			reason := "uncertain"
			var refused DecisionError
			if errors.As(err, &refused) {
				reason = "comparison-failed"
			}
			_ = db.StopVerification(stopCtx, claim, reason)
		}
	}()
	fence := func() error {
		current, _, _, e := CurrentProof(ctx, db, load, id)
		if e != nil || current != c || db.CheckVerificationClaim(ctx, claim) != nil {
			return verification.ErrVerification
		}
		return nil
	}
	if fence() != nil {
		return proof, ControllerError{stage}
	}
	stage = "source"
	sourceClient, e := sourcefetch.New(c.Repository, "")
	if e != nil {
		return proof, ControllerError{stage}
	}
	source, e := sourceClient.Fetch(ctx, c.SourceSHA)
	if e != nil {
		return proof, ControllerError{stage}
	}
	create := func(kind string, apply func() (string, error)) error {
		if fence() != nil {
			return verification.ErrVerification
		}
		effect, e := db.BeginVerificationEffect(ctx, claim, kind)
		if e != nil {
			return e
		}
		if fence() != nil {
			unsentCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
			// Cleanup requires an observable durable cancellation. A restart
			// must never infer no-send merely from the missing UID.
			_ = db.CancelUnsentVerificationEffect(unsentCtx, effect)
			stop()
			return verification.ErrVerification
		}
		uid, e := apply()
		if uid != "" {
			if kind == "config" {
				configUID = uid
			} else {
				pods[kind] = uid
			}
		}
		if e != nil {
			return e
		}
		if db.AcknowledgeVerificationEffect(ctx, effect, uid) != nil {
			return verification.ErrVerification
		}
		return fence()
	}
	stage = "recipe"
	inputs, e := verification.Prepare(c.Repository, c.Recipe, snapshot.Issue.Body, source, record.Delta)
	if e != nil {
		return proof, ControllerError{stage}
	}
	if c.Recipe == repositorytests.Recipe {
		// The settings come from the manifest the current policy binds; the
		// command's authority revision covers that digest, and every fence
		// re-checks it.
		p, _, _, e := load()
		m, ok := project.Lookup(c.Repository)
		if e != nil || !ok || m.Verification == nil || p.Revision() != c.AuthorityRevision || p.ProjectSHA256 == "" || m.Digest() != p.ProjectSHA256 {
			return proof, ControllerError{stage}
		}
		v := *m.Verification
		trees, e := repositorytests.BuildTrees(source, inputs.Delta(), v)
		if e != nil {
			return proof, ControllerError{stage}
		}
		testRuns = map[string]*podtransport.TestsRun{}
		for _, run := range repositorytests.RunNames() {
			bundle, _ := trees.Tree(run)
			raw, e := workspacebundle.Encode(bundle)
			tree, treeErr := bundle.TreeDigest()
			if e != nil || treeErr != nil {
				return proof, ControllerError{stage}
			}
			p := repositorytests.Pod{Namespace: podtransport.VerifierNamespace, MailboxImage: mailboxImage, VerificationID: c.ID(), WorkItemID: c.WorkItemID,
				Run: run, SourceSHA: c.SourceSHA, TreeSHA256: tree, Verification: v}
			planned, e := podtransport.PlanTestsRun(client, p, raw, time.UnixMilli(c.ExpiresAt))
			if e != nil {
				return proof, ControllerError{stage}
			}
			testRuns[repositorytests.LedgerKind(run)] = planned
		}
		stage = "config"
		if create("config", func() (string, error) { return testRuns["baseline"].CreateConfig(ctx) }) != nil {
			return proof, ControllerError{stage}
		}
		// The runs take minutes (image pulls, three concurrent pods, repeated
		// candidate runs), so the claim is renewed while they run; the work
		// stays bounded by the command's expiry and each pod's deadline.
		claimCancel()
		var bounded context.CancelFunc
		ctx, bounded = context.WithDeadline(parent, time.UnixMilli(c.ExpiresAt))
		defer bounded()
		renew := func() error {
			if time.Until(claim.Until) > 120*time.Second {
				return nil
			}
			renewed, e := db.RenewVerificationClaim(ctx, claim)
			if e != nil {
				return e
			}
			claim = renewed
			return fence()
		}
		for _, run := range repositorytests.RunNames() {
			kind := repositorytests.LedgerKind(run)
			stage = kind
			if create(kind, func() (string, error) { return testRuns[kind].CreatePod(ctx, configUID) }) != nil {
				return proof, ControllerError{stage}
			}
		}
		deadline := time.Now()
		for _, run := range repositorytests.RunNames() {
			deadline = maxTime(deadline, time.Now().Add(time.Duration(repositorytests.DeadlineSeconds(run, v))*time.Second))
		}
		imported, finished := map[string]bool{}, map[string]bool{}
		for len(finished) < len(testRuns) {
			if time.Now().After(deadline) || renew() != nil {
				return proof, ControllerError{stage}
			}
			for _, run := range repositorytests.RunNames() {
				kind := repositorytests.LedgerKind(run)
				if finished[kind] {
					continue
				}
				stage = kind
				status, e := testRuns[kind].Status(ctx, pods[kind])
				if e != nil {
					return proof, ControllerError{stage}
				}
				switch {
				case status.Ready && !imported[kind]:
					// Each run imports as soon as it is ready: its runner
					// waits for the marker only a bounded time.
					if fence() != nil || testRuns[kind].Import(ctx, pods[kind]) != nil {
						return proof, ControllerError{stage}
					}
					imported[kind] = true
				case status.Succeeded && imported[kind]:
					finished[kind] = true
				case status.Succeeded:
					return proof, ControllerError{stage}
				}
			}
			select {
			case <-ctx.Done():
				return proof, ControllerError{stage}
			case <-time.After(time.Second):
			}
		}
		stage = "results"
		results := map[string][]repositorytests.Run{}
		logs := map[string]string{}
		for _, run := range repositorytests.RunNames() {
			kind := repositorytests.LedgerKind(run)
			if renew() != nil || fence() != nil {
				return proof, ControllerError{stage}
			}
			runs, raw, e := testRuns[kind].Collect(ctx, pods[kind])
			if e != nil {
				return proof, ControllerError{stage}
			}
			results[run], logs[kind] = runs, authority.Digest(raw)
		}
		stage = "comparison"
		decision := repositorytests.Decide(v, repositorytests.Evidence{Baseline: results[repositorytests.RunBaseline][0], Cross: results[repositorytests.RunCross][0],
			Candidate: results[repositorytests.RunCandidate], Changes: trees.Changes})
		if !decision.Passed {
			// A refusal is recorded only under current authority.
			if fence() != nil {
				return proof, ControllerError{"authority"}
			}
			return proof, DecisionError{Reason: decision.Reason}
		}
		if fence() != nil {
			return proof, ControllerError{"authority"}
		}
		stage = "cleanup"
		if cleanup() != nil {
			return proof, ControllerError{stage}
		}
		if fence() != nil {
			return proof, ControllerError{"authority"}
		}
		uids := map[string]string{"config": configUID}
		for kind, uid := range pods {
			uids[kind] = uid
		}
		raw, _ := json.Marshal(decision)
		proof = BehaviorProof{Version: BehaviorProofV2, Command: c, ToolchainImage: v.Image, BaselineResultSHA256: logs["baseline"], CandidateResultSHA256: logs["candidate"],
			CrossResultSHA256: logs["tests"], Decision: raw, ResourceUIDs: uids, BehaviorVerified: true, PublicationAuthorized: false}
		encoded, _ := json.Marshal(proof)
		if db.FinishVerification(ctx, claim, encoded) != nil {
			return BehaviorProof{}, ControllerError{"commit"}
		}
		return proof, nil
	}
	resources, e = podtransport.PlanVerification(client, c, inputs, image)
	if e != nil {
		return proof, ControllerError{stage}
	}
	stage = "config"
	if create("config", func() (string, error) { return resources.CreateConfig(ctx) }) != nil {
		return proof, ControllerError{stage}
	}
	for _, kind := range []string{"baseline", "candidate", "reference", "tests"} {
		stage = kind
		if create(kind, func() (string, error) { return resources.CreatePod(ctx, kind, configUID) }) != nil {
			return proof, ControllerError{stage}
		}
	}
	stage = "results"
	if fence() != nil {
		return proof, ControllerError{stage}
	}
	results, e := resources.ReadResults(ctx, pods)
	if e != nil {
		return proof, ControllerError{stage}
	}
	stage = "comparison"
	if verification.CompareRecipe(c.Recipe, results.Baseline, results.Candidate, results.Reference) != nil {
		return proof, ControllerError{stage}
	}
	if fence() != nil {
		return proof, ControllerError{"authority"}
	}
	stage = "cleanup"
	if cleanup() != nil {
		return proof, ControllerError{stage}
	}
	if fence() != nil {
		return proof, ControllerError{"authority"}
	}
	uids := map[string]string{"config": configUID}
	for kind, uid := range pods {
		uids[kind] = uid
	}
	proof = BehaviorProof{Version: BehaviorProofV1, Command: c, ToolchainImage: image, BaselineResultSHA256: authority.Digest(results.Baseline), CandidateResultSHA256: authority.Digest(results.Candidate), ReferenceResultSHA256: authority.Digest(results.Reference), ResourceUIDs: uids, BehaviorVerified: true, PublicationAuthorized: false}
	raw, _ := json.Marshal(proof)
	if db.FinishVerification(ctx, claim, raw) != nil {
		return BehaviorProof{}, ControllerError{"commit"}
	}
	return proof, nil
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
