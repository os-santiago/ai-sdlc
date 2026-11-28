package verificationflow

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/repositorytests"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/secretscan"
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

// DecisionError is a verified refusal of the candidate: the repository-tests
// rules decided after their runs completed, or the candidate baseline refused
// it before any run. The reason is the decision, not an uncertainty.
type DecisionError struct {
	Reason string
	// Secret is set only for the "candidate-secret" refusal. It names the
	// rule and path class, never the matched text or the path.
	Secret secretscan.Finding
}

// SecretRefusal is the reason of a candidate that adds a credential.
const SecretRefusal = "candidate-secret"

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
// Its pods run in the default sandbox RuntimeClass.
func Run(ctx context.Context, db *store.DB, load PolicyLoader, id string, client *podtransport.Client, image string) (proof BehaviorProof, err error) {
	return RunWithMailbox(ctx, db, load, id, client, image, "", executor.DefaultRuntimeClass, podtransport.RuntimeClassAdmission{})
}

// RunWithMailbox also receives the installation's pinned mailbox image, which
// carries the repository-tests runner and transfer helper, and the installed
// sandbox RuntimeClass every verification pod runs in, with what the preflight
// observed that class's admission adds to each pod.
// It reads source anonymously; Runtime.PollOnce uses the installation's reader.
func RunWithMailbox(ctx context.Context, db *store.DB, load PolicyLoader, id string, client *podtransport.Client, image, mailboxImage, runtimeClass string, admission podtransport.RuntimeClassAdmission) (proof BehaviorProof, err error) {
	return runWithMailbox(ctx, db, load, id, client, image, mailboxImage, runtimeClass, admission, sourcefetch.Reader{})
}

// runWithMailbox fetches the pinned source with reader: anonymously, or with
// the installation's repository-scoped read credential, which stays in this
// controller and never reaches a pod, ConfigMap, proof or log.
func runWithMailbox(ctx context.Context, db *store.DB, load PolicyLoader, id string, client *podtransport.Client, image, mailboxImage, runtimeClass string, admission podtransport.RuntimeClassAdmission, reader sourcefetch.Reader) (proof BehaviorProof, err error) {
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
				// Keep the stage that stopped the run for the diagnostic.
				err = cleanupAfter{cause: e, stage: stage}
			}
		}
		if err != nil {
			proof = BehaviorProof{}
			stopCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
			defer stop()
			reason := "uncertain"
			var refused DecisionError
			// A refusal is recorded only while the claim is still valid; the
			// cleanup above may have outlived it.
			if errors.As(err, &refused) && db.CheckVerificationClaim(stopCtx, claim) == nil {
				reason = "comparison-failed"
			}
			if db.StopVerification(stopCtx, claim, reason) != nil && reason != "uncertain" {
				// The claim lapsed between the check and the update.
				_ = db.StopVerification(stopCtx, claim, "uncertain")
			}
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
	source, e := reader.Fetch(ctx, c.Repository, c.SourceSHA)
	if e != nil {
		stage = sourceStage(e)
		return proof, ControllerError{stage}
	}
	create := func(kind string, apply func() (string, error)) error {
		if fence() != nil {
			return createStop{"fence"}
		}
		effect, e := db.BeginVerificationEffect(ctx, claim, kind)
		if e != nil {
			return createStop{"ledger"}
		}
		if fence() != nil {
			unsentCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
			// Cleanup requires an observable durable cancellation. A restart
			// must never infer no-send merely from the missing UID.
			_ = db.CancelUnsentVerificationEffect(unsentCtx, effect)
			stop()
			return createStop{"fence"}
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
			// The create response named the object even though a later check
			// failed: record that UID, or cleanup could never remove it and
			// the verifier would stay blocked.
			if uid != "" {
				ackCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
				_ = db.AcknowledgeVerificationEffect(ackCtx, effect, uid)
				stop()
			}
			return createStop{"create"}
		}
		if db.AcknowledgeVerificationEffect(ctx, effect, uid) != nil {
			return createStop{"acknowledge"}
		}
		if fence() != nil {
			return createStop{"fence"}
		}
		return nil
	}
	stage = "recipe"
	inputs, e := verification.Prepare(c.Repository, c.Recipe, snapshot.Issue.Body, source, record.Delta)
	if e != nil {
		return proof, ControllerError{stage}
	}
	// The candidate baseline runs before any pod exists, so a candidate that
	// adds a credential is refused without running its code, and the
	// independent reviewer never receives it. Only the finding's rule and
	// path class leave the scanner.
	if refused := candidateBaseline(source, inputs.Delta()); refused != nil {
		// A refusal is recorded only under current authority.
		if fence() != nil {
			return proof, ControllerError{"authority"}
		}
		return proof, refused
	}
	if c.Recipe == repositorytests.Recipe {
		// The settings come from the manifest the current policy binds; the
		// command's authority revision covers that digest, and every fence
		// re-checks it.
		p, _, _, e := load()
		if e != nil || p.Revision() != c.AuthorityRevision || p.ProjectSHA256 == "" {
			return proof, ControllerError{stage}
		}
		m, ok := project.Bound(c.Repository, p.ProjectSHA256)
		if !ok || m.Verification == nil {
			return proof, ControllerError{stage}
		}
		v := *m.Verification
		trees, e := repositorytests.BuildTrees(source, inputs.Delta(), v)
		if e != nil {
			return proof, ControllerError{stage}
		}
		// A manifest with dependencies reads the sealed cache of the base
		// tree's dependency files. Candidates cannot change those files
		// (they are harness), so every run reads the same cache.
		cache, e := dependencyCache(ctx, client, v, trees.Baseline)
		if e != nil {
			stage = e.Error()
			return proof, ControllerError{stage}
		}
		testRuns = map[string]*podtransport.TestsRun{}
		for _, run := range repositorytests.RunNames() {
			bundle, _ := trees.Tree(run)
			tree, treeErr := bundle.TreeDigest()
			if treeErr != nil {
				return proof, ControllerError{stage}
			}
			p := repositorytests.Pod{Namespace: podtransport.VerifierNamespace, MailboxImage: mailboxImage, RuntimeClass: runtimeClass, VerificationID: c.ID(), WorkItemID: c.WorkItemID,
				Run: run, SourceSHA: c.SourceSHA, TreeSHA256: tree, Verification: v, DependencyCache: cache}
			// The run shares the tree's bytes with source and encodes it only
			// while importing, so three runs do not hold three wire copies.
			planned, e := podtransport.PlanTestsRunTree(client, p, admission, bundle, time.UnixMilli(c.ExpiresAt))
			if e != nil {
				return proof, ControllerError{stage}
			}
			testRuns[repositorytests.LedgerKind(run)] = planned
		}
		stage = "config"
		if e := create("config", func() (string, error) { return testRuns["baseline"].CreateConfig(ctx) }); e != nil {
			stage = createStage(stage, e)
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
			if time.Until(claim.Until) > 150*time.Second {
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
			if e := create(kind, func() (string, error) { return testRuns[kind].CreatePod(ctx, configUID) }); e != nil {
				stage = createStage(stage, e)
				return proof, ControllerError{stage}
			}
		}
		deadline := time.Now()
		for _, run := range repositorytests.RunNames() {
			deadline = maxTime(deadline, time.Now().Add(time.Duration(repositorytests.DeadlineSeconds(run, v))*time.Second))
		}
		// Every stop in this loop names a bounded step: runs-* for the
		// whole set of runs, or the run and its step (for example
		// candidate-import-transfer-timeout). The stop itself is unchanged,
		// and the stage is kept for a failed cleanup's diagnostic.
		imported, finished := map[string]bool{}, map[string]bool{}
		for len(finished) < len(testRuns) {
			if time.Now().After(deadline) {
				stage = "runs-deadline"
				return proof, ControllerError{stage}
			}
			if renew() != nil {
				stage = "runs-renew"
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
					stage = testsStage(stage, e)
					return proof, ControllerError{stage}
				}
				switch {
				case status.Ready && !imported[kind]:
					// Each run imports as soon as it is ready: its runner
					// waits for the marker only a bounded time.
					// Renew first so the claim covers this whole import.
					if renew() != nil {
						stage += "-import-renew"
						return proof, ControllerError{stage}
					}
					if fence() != nil {
						stage += "-import-fence"
						return proof, ControllerError{stage}
					}
					if e := testRuns[kind].Import(ctx, pods[kind]); e != nil {
						stage = testsStage(stage, e)
						return proof, ControllerError{stage}
					}
					imported[kind] = true
				case status.Succeeded && imported[kind]:
					finished[kind] = true
				case status.Succeeded:
					stage += "-unimported-success"
					return proof, ControllerError{stage}
				}
			}
			select {
			case <-ctx.Done():
				stage = "runs-context"
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
		if db.FinishVerificationWith(ctx, claim, encoded, []string{"baseline", "candidate", "config", "tests"}) != nil {
			return BehaviorProof{}, ControllerError{"commit"}
		}
		return proof, nil
	}
	resources, e = podtransport.PlanVerification(client, c, inputs, image, runtimeClass, admission)
	if e != nil {
		return proof, ControllerError{stage}
	}
	stage = "config"
	if e := create("config", func() (string, error) { return resources.CreateConfig(ctx) }); e != nil {
		stage = createStage(stage, e)
		return proof, ControllerError{stage}
	}
	for _, kind := range []string{"baseline", "candidate", "reference", "tests"} {
		stage = kind
		if e := create(kind, func() (string, error) { return resources.CreatePod(ctx, kind, configUID) }); e != nil {
			stage = createStage(stage, e)
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
