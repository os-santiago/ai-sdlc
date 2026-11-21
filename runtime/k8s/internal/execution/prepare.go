package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
	"time"
	"unicode/utf8"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/candidate"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/intake"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/normalizedevent"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/sourcefetch"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// PreparationInputs are current installation-owned inputs, never issue fields.
// The loader must reread reviewed configuration on every call; revoking a
// profile means removing/changing it or changing authority, not caching it.
// Values must remain stable while this call copies them; later reloads may reuse
// and mutate their own buffers. I/O must honor the supplied context.
type PreparationInputs struct {
	Policy        authority.Policy
	Intake        intake.Config
	Profile, ADev []byte
}
type PreparationLoader func(context.Context) (PreparationInputs, error)

// Prepared contains verified immutable bytes, not permission to create a Pod.
// A controller must first persist lifecycle ownership and revalidate at each
// external-effect boundary. Accessors return copies, never internal buffers.
type Prepared struct {
	assignment, source, originalSource, profile, adev []byte
	claim                                             store.ExecutionClaim
}

func (p Prepared) Assignment() []byte          { return append([]byte(nil), p.assignment...) }
func (p Prepared) Source() []byte              { return append([]byte(nil), p.source...) }
func (p Prepared) Profile() []byte             { return append([]byte(nil), p.profile...) }
func (p Prepared) ADev() []byte                { return append([]byte(nil), p.adev...) }
func (p Prepared) Claim() store.ExecutionClaim { return p.claim }

type sourceLoader func(context.Context, string, string) (workspacebundle.Bundle, error)

// Prepare fetches only the repository and SHA verified from the signed claim.
// The optional source credential is trusted installation configuration and is
// never placed in the preparation, assignment, model input or returned error.
func Prepare(ctx context.Context, db *store.DB, load PreparationLoader, claim store.ExecutionClaim, sourceCredential string) (Prepared, error) {
	return prepare(ctx, db, load, claim, func(ctx context.Context, repository, sha string) (workspacebundle.Bundle, error) {
		client, err := sourcefetch.New(repository, sourceCredential)
		if err != nil {
			return workspacebundle.Bundle{}, ErrAuthority
		}
		return client.Fetch(ctx, sha)
	})
}

func checkInputs(in PreparationInputs, c Command) error {
	p, err := executor.ParseProfile(in.Profile, c.ProfileSHA256)
	if err != nil || len(in.ADev) == 0 || len(in.ADev) > 64*1024 || !utf8.Valid(in.ADev) || workspacebundle.Digest(in.ADev) != p.ADevSHA256 {
		return ErrAuthority
	}
	return nil
}
func copyInputs(in PreparationInputs) PreparationInputs {
	in.Profile = append([]byte(nil), in.Profile...)
	in.ADev = append([]byte(nil), in.ADev...)
	keys := make(map[string]authority.PublicKey, len(in.Policy.Keys))
	for id, key := range in.Policy.Keys {
		keys[id] = key
	}
	in.Policy.Keys = keys
	in.Policy.Grants = append([]authority.Grant(nil), in.Policy.Grants...)
	in.Intake.AuthorizedActorIDs = append([]int64(nil), in.Intake.AuthorizedActorIDs...)
	return in
}
func verifyRecord(p authority.Policy, r store.ExecutionRequest) (Command, error) {
	var receipt authority.Receipt
	if authority.DecodeStrict(r.Receipt, &receipt) != nil || authority.Digest(r.Payload) != r.PayloadDigest {
		return Command{}, ErrAuthority
	}
	c, err := Verify(p, receipt, r.Payload, time.Now())
	if err != nil || c.ID() != r.ID || c.WorkItemID != r.WorkItem || c.Attempt != r.Attempt || c.Repository != r.Repository ||
		c.AuthorityRevision != r.AuthorityRevision || !time.UnixMilli(c.ExpiresAt).Equal(r.ExpiresAt) {
		return Command{}, ErrAuthority
	}
	return c, nil
}
func preparationProof(ctx context.Context, db *store.DB, load PreparationLoader, claim store.ExecutionClaim) (Command, intake.Snapshot, PreparationInputs, error) {
	return preparationProofStatus(ctx, db, load, claim, "ready")
}
func preparationProofStatus(ctx context.Context, db *store.DB, load PreparationLoader, claim store.ExecutionClaim, status string) (Command, intake.Snapshot, PreparationInputs, error) {
	fail := func() (Command, intake.Snapshot, PreparationInputs, error) {
		return Command{}, intake.Snapshot{}, PreparationInputs{}, ErrAuthority
	}
	if db == nil || load == nil || ctx.Err() != nil || !claim.Until.After(time.Now()) {
		return fail()
	}
	r, err := db.ReadClaimedExecution(ctx, claim)
	if err != nil {
		return fail()
	}
	in, err := load(ctx)
	if err != nil {
		return fail()
	}
	if len(in.Profile) == 0 || len(in.Profile) > executor.MaxProfileBytes || len(in.ADev) == 0 || len(in.ADev) > 64*1024 {
		return fail()
	}
	in = copyInputs(in)
	c, err := verifyRecord(in.Policy, r)
	if err != nil || checkInputs(in, c) != nil {
		return fail()
	}
	if c.Repair != nil {
		if repairInputs(ctx, db, in, c) != nil {
			return fail()
		}
		status = "running"
	}
	snapshot, err := verifiedSnapshotStatus(ctx, db, in.Policy, in.Intake, c, status)
	if err != nil {
		return fail()
	}
	// Reload after the bounded proof query, and check the durable token again.
	current, err := load(ctx)
	if err != nil || current.Policy.Revision() != in.Policy.Revision() || current.Intake.Digest() != in.Intake.Digest() ||
		!bytes.Equal(current.Profile, in.Profile) || !bytes.Equal(current.ADev, in.ADev) {
		return fail()
	}
	if _, err = verifyRecord(current.Policy, r); err != nil {
		return fail()
	}
	if _, err = db.ReadClaimedExecution(ctx, claim); err != nil {
		return fail()
	}
	latest, err := load(ctx)
	if err != nil || latest.Policy.Revision() != in.Policy.Revision() || latest.Intake.Digest() != in.Intake.Digest() ||
		!bytes.Equal(latest.Profile, in.Profile) || !bytes.Equal(latest.ADev, in.ADev) || ctx.Err() != nil || !claim.Until.After(time.Now()) {
		return fail()
	}
	if _, err = verifyRecord(latest.Policy, r); err != nil {
		return fail()
	}
	return c, snapshot, in, nil
}
func prepare(ctx context.Context, db *store.DB, load PreparationLoader, claim store.ExecutionClaim, fetch sourceLoader) (result Prepared, err error) {
	if db == nil || load == nil || fetch == nil {
		return Prepared{}, ErrAuthority
	}
	deadline := time.Now().Add(45 * time.Second)
	if claim.Until.Before(deadline) {
		deadline = claim.Until
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	defer func() {
		if err != nil {
			cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
			defer stop()
			_ = db.StopExecutionClaim(cleanup, claim, "aborted")
		}
	}()
	c, snapshot, in, err := preparationProof(ctx, db, load, claim)
	if err != nil {
		return Prepared{}, ErrAuthority
	}
	assignment := executor.Assignment{Version: executor.AssignmentVersion, WorkItemID: c.WorkItemID, Attempt: c.Attempt,
		Repository: c.Repository, SourceSHA: c.SourceSHA, TreeSHA256: c.ProfileSHA256, ProfileSHA256: c.ProfileSHA256,
		AuthorityRevision: c.AuthorityRevision, IssueNumbers: []int{c.IssueNumber}, Title: snapshot.Issue.Title, Body: snapshot.Issue.Body,
		AcceptanceCriteria: normalizedevent.ExtractAcceptanceCriteria(snapshot.Issue.Body), ExpiresAt: c.ExpiresAt}
	assignment.Workspace = WorkspaceFor(c.Repository)
	var parent store.VerificationCandidate
	if c.Repair != nil {
		a, p, e := repairPreparation(ctx, db, in.Policy, c)
		if e != nil {
			return Prepared{}, ErrAuthority
		}
		assignment.Repair = &a
		assignment.TreeSHA256 = a.Reference.ParentTreeSHA256
		parent = p
	}
	// Reject unsupported issue shape before any network acquisition. The temporary
	// tree placeholder is replaced by the verified source identity below.
	initial, _ := json.Marshal(assignment)
	if _, err = executor.ParseAssignment(initial, workspacebundle.Digest(initial), time.Now()); err != nil {
		return Prepared{}, ErrAuthority
	}
	bundle, err := fetch(ctx, c.Repository, c.SourceSHA)
	if err != nil || bundle.SourceSHA != c.SourceSHA || bundle.Validate() != nil || ctx.Err() != nil {
		return Prepared{}, ErrAuthority
	}
	originalSource, err := workspacebundle.Encode(bundle)
	if err != nil {
		return Prepared{}, ErrAuthority
	}
	if c.Repair != nil {
		originalTree, e := bundle.TreeDigest()
		if e != nil || originalTree != c.Repair.OriginalTreeSHA256 {
			return Prepared{}, ErrAuthority
		}
		delta, e := candidate.Decode(parent.Delta)
		if e != nil || workspacebundle.Digest(parent.Delta) != parent.Record.DeltaSHA256 {
			return Prepared{}, ErrAuthority
		}
		bundle, e = candidate.Apply(bundle, delta)
		if e != nil {
			return Prepared{}, ErrAuthority
		}
	}
	source, err := workspacebundle.Encode(bundle)
	if err != nil {
		return Prepared{}, ErrAuthority
	}
	assignment.TreeSHA256, err = bundle.TreeDigest()
	if err != nil || (c.Repair != nil && assignment.TreeSHA256 != c.Repair.ParentTreeSHA256) {
		return Prepared{}, ErrAuthority
	}
	raw, err := json.Marshal(assignment)
	if err != nil {
		return Prepared{}, ErrAuthority
	}
	current, _, after, err := preparationProof(ctx, db, load, claim)
	if err != nil || current.ID() != c.ID() || !bytes.Equal(after.Profile, in.Profile) || !bytes.Equal(after.ADev, in.ADev) {
		return Prepared{}, ErrAuthority
	}
	if _, err = executor.ParseAssignment(raw, workspacebundle.Digest(raw), time.Now()); err != nil || ctx.Err() != nil {
		return Prepared{}, ErrAuthority
	}
	return Prepared{assignment: raw, source: source, originalSource: originalSource, profile: in.Profile, adev: in.ADev, claim: claim}, nil
}

// WorkspaceFor marks the assignment of a repository verified with its own
// tests, whose generator workspace cannot build or test: the prompt then says
// so as reviewed installation context. Without it, agents spend their step
// budget on failing test commands.
func WorkspaceFor(repository string) string {
	m, ok := project.Lookup(repository)
	if !ok || m.Verification == nil {
		return ""
	}
	return executor.WorkspaceOfflineTests
}
