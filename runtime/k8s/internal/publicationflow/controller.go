package publicationflow

import (
	"context"
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/publication"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/sourcefetch"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verificationflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
	"time"
)

// Transport is trusted implementation code. Installed runtimes must construct
// publication.Writer from their fixed credential; no issue supplies a transport.
type Transport interface {
	CreateTree(context.Context, publication.WriteFence, publication.TreePlan) (string, error)
	CreateCommit(context.Context, publication.WriteFence, publication.TreePlan, string, time.Time) (string, error)
	CreateBranch(context.Context, publication.WriteFence, publication.TreePlan, string) error
	CreatePullRequest(context.Context, publication.WriteFence, publication.TreePlan, string, string, string) (publication.PublishedPullRequest, error)
}
type Proof = store.PublicationProof

type ControllerError struct{ Stage string }

func (e ControllerError) Error() string { return "publication stopped at " + e.Stage }

type SourceLoader func(context.Context, string, string) (workspacebundle.Bundle, error)

func fetchSource(ctx context.Context, repo, sha string) (workspacebundle.Bundle, error) {
	client, e := sourcefetch.New(repo, "")
	if e != nil {
		return workspacebundle.Bundle{}, e
	}
	return client.Fetch(ctx, sha)
}

// Run takes only an identifier from the signed durable queue. No branch, patch,
// model answer, target URL or permission can be supplied by an external caller.
func Run(ctx context.Context, db *store.DB, policy verificationflow.PolicyLoader, review ReviewLoader, id string, writer Transport) (Proof, error) {
	return run(ctx, db, policy, review, id, writer, fetchSource)
}
func run(ctx context.Context, db *store.DB, policy verificationflow.PolicyLoader, review ReviewLoader, id string, writer Transport, fetch SourceLoader) (proof Proof, err error) {
	if db == nil || policy == nil || review == nil || writer == nil || fetch == nil {
		return proof, ControllerError{"configuration"}
	}
	ctx, cancel := context.WithTimeout(ctx, 160*time.Second)
	defer cancel()
	c, record, e := CurrentProof(ctx, db, policy, review, id)
	if e != nil {
		return proof, ControllerError{"authority"}
	}
	prepareCtx, stop := context.WithTimeout(ctx, 40*time.Second)
	source, e := fetch(prepareCtx, c.Repository, c.SourceSHA)
	stop()
	if e != nil {
		return proof, ControllerError{"source"}
	}
	plan, e := publication.PrepareTree(c, source, record.Delta)
	if e != nil {
		return proof, ControllerError{"tree-plan"}
	}
	raw, _ := json.Marshal(struct {
		Command publication.Command
		Plan    publication.TreePlan
	}{c, plan})
	planDigest := authority.Digest(raw)
	current, _, e := CurrentProof(ctx, db, policy, review, id)
	if e != nil || current != c {
		return proof, ControllerError{"authority"}
	}
	claim, e := db.ClaimPublication(ctx, id, planDigest)
	if e != nil {
		return proof, ControllerError{"claim"}
	}
	ctx, stopClaim := context.WithDeadline(ctx, claim.Until)
	defer stopClaim()
	stage := "authority"
	defer func() {
		if recover() != nil {
			err = ControllerError{stage}
		}
		if err != nil {
			proof = Proof{}
			cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
			defer stop()
			_ = db.StopPublication(cleanup, claim, "uncertain")
		}
	}()
	fence := func(ctx context.Context) error {
		current, _, e := CurrentProof(ctx, db, policy, review, id)
		if e != nil || current != c {
			return publication.ErrPublication
		}
		return db.CheckPublication(ctx, claim)
	}
	effect := func(kind string, invoke func(publication.WriteFence) (store.PublicationReceipt, error)) error {
		var intent store.PublicationEffect
		calls := 0
		transportFence := func(ctx context.Context, digest string) error {
			if !workspacebundle.ValidDigest(digest) {
				return publication.ErrPublication
			}
			calls++
			if calls == 1 {
				if fence(ctx) != nil {
					return publication.ErrPublication
				}
				var e error
				intent, e = db.BeginPublicationEffect(ctx, claim, kind, digest)
				if e != nil {
					return e
				}
				if fence(ctx) != nil {
					cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
					defer stop()
					// Still in the writer's pre-send callback: transport has not started.
					_ = db.SealPublicationNotSent(cleanup, intent)
					return publication.ErrPublication
				}
				return nil
			}
			if calls != 2 || intent.RequestDigest != digest {
				return publication.ErrPublication
			}
			return fence(ctx)
		}
		receipt, e := invoke(transportFence)
		if e != nil || calls != 2 || intent.Token == "" || receipt.Kind != kind {
			return publication.ErrPublication
		}
		if fence(ctx) != nil || db.AcknowledgePublicationEffect(ctx, intent, receipt) != nil {
			return publication.ErrPublication
		}
		return fence(ctx)
	}
	stage = "tree"
	if effect("tree", func(f publication.WriteFence) (store.PublicationReceipt, error) {
		sha, e := writer.CreateTree(ctx, f, plan)
		if e != nil || sha != plan.ExpectedTreeSHA {
			return store.PublicationReceipt{}, publication.ErrPublication
		}
		return store.PublicationReceipt{Version: "ai-sdlc.publication-receipt/v1", Kind: "tree", SHA: sha}, nil
	}) != nil {
		return proof, ControllerError{stage}
	}
	stage = "commit"
	commit := ""
	if effect("commit", func(f publication.WriteFence) (store.PublicationReceipt, error) {
		var e error
		commit, e = writer.CreateCommit(ctx, f, plan, c.SourceSHA, claim.CommitTime)
		if e != nil || !workspacebundle.ValidSourceSHA(commit) {
			return store.PublicationReceipt{}, publication.ErrPublication
		}
		return store.PublicationReceipt{Version: "ai-sdlc.publication-receipt/v1", Kind: "commit", SHA: commit}, nil
	}) != nil {
		return proof, ControllerError{stage}
	}
	stage = "branch"
	if effect("branch", func(f publication.WriteFence) (store.PublicationReceipt, error) {
		if e := writer.CreateBranch(ctx, f, plan, commit); e != nil {
			return store.PublicationReceipt{}, e
		}
		return store.PublicationReceipt{Version: "ai-sdlc.publication-receipt/v1", Kind: "branch", SHA: commit, Branch: plan.Branch}, nil
	}) != nil {
		return proof, ControllerError{stage}
	}
	stage = "pull-request"
	number := 0
	if effect("pull-request", func(f publication.WriteFence) (store.PublicationReceipt, error) {
		pr, e := writer.CreatePullRequest(ctx, f, plan, c.SourceSHA, commit, planDigest)
		if e != nil || pr.HeadSHA != commit || pr.Branch != plan.Branch || pr.Number < 1 {
			return store.PublicationReceipt{}, publication.ErrPublication
		}
		number = pr.Number
		return store.PublicationReceipt{Version: "ai-sdlc.publication-receipt/v1", Kind: "pull-request", SHA: commit, Branch: plan.Branch, PullRequest: number}, nil
	}) != nil {
		return proof, ControllerError{stage}
	}
	stage = "proof"
	evidence, e := db.ReadPublicationEffects(ctx, claim)
	if e != nil || fence(ctx) != nil {
		return proof, ControllerError{stage}
	}
	raw, _ = json.Marshal(evidence)
	proof = Proof{Version: "ai-sdlc.publication-proof/v1", PublicationID: id, PlanSHA256: planDigest, EffectsSHA256: authority.Digest(raw), CommitSHA: commit, Branch: plan.Branch, PullRequest: number}
	raw, _ = json.Marshal(proof)
	if db.FinishPublication(ctx, claim, raw) != nil {
		return Proof{}, ControllerError{"commit-proof"}
	}
	return proof, nil
}

type Runtime struct {
	DB     *store.DB
	Policy verificationflow.PolicyLoader
	Review ReviewLoader
	Writer Transport
}

func (r Runtime) PollOnce(ctx context.Context) (string, error) {
	if r.DB == nil || r.Policy == nil || r.Review == nil || r.Writer == nil {
		return "", ControllerError{"configuration"}
	}
	if e := r.DB.ExpirePublications(ctx); e != nil {
		return "", e
	}
	blocked, e := r.DB.PublicationBlocked(ctx)
	if e != nil {
		return "", e
	}
	if blocked {
		return "publication-quarantined", nil
	}
	id, e := r.DB.NextPublication(ctx)
	if e != nil || id == "" {
		return "publication-idle", e
	}
	if _, e = Run(ctx, r.DB, r.Policy, r.Review, id, r.Writer); e != nil {
		return "publication-stopped", e
	}
	return "pull-request-published", nil
}
