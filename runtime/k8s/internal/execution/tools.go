package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync/atomic"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/generatortools"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/tektontasks"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// toolSession is the part of the executor session that carries generator tool
// calls: the fixed read-tool and respond-tool mailbox helpers.
type toolSession interface {
	ReadToolRequest(context.Context) (modelmailbox.Envelope, error)
	PublishToolResponse(context.Context, modelmailbox.Envelope) error
}

// ToolRunner runs one authorized-shape tool call, normally
// tektontasks.InvokeTool bound to the manifest, the generation's base revision,
// its budget and journal, and the sandbox runtime class.
type ToolRunner func(context.Context, tektontasks.ToolCall) (tektontasks.ToolResult, error)

// toolDispatch answers the generator's tool calls in order. A call is run at
// most once: the exact last request may be observed again until the mailbox
// consumes its answer. Sequences may skip forward (the mailbox, the only
// writer of these files, numbers calls that ended unread, for example after
// their window passed); a repeated, rewritten or earlier sequence stops the run.
type toolDispatch struct {
	session toolSession
	run     ToolRunner
	// authorize is rechecked while a tool runs; a refusal cancels the run.
	authorize func(context.Context) error
	previous  modelmailbox.Envelope
}

// toolAnswerMargin is how long before the request expires the controller
// stops a run, so its answer (or failure) reaches the mailbox in time.
const toolAnswerMargin = 10 * time.Second

// toolAuthorizeEvery is how often authority is rechecked while a tool runs.
var toolAuthorizeEvery = 10 * time.Second

// poll answers one pending tool call, if any. The generator waits for the
// answer, so no model request is pending while a tool runs.
func (d *toolDispatch) poll(ctx context.Context) error {
	request, err := d.session.ReadToolRequest(ctx)
	if errors.Is(err, modelmailbox.ErrPending) {
		return nil
	}
	if err != nil {
		return driveStop{"execution-tool-read"}
	}
	if d.previous.Sequence != 0 && request.Sequence == d.previous.Sequence {
		if !sameRequest(request, d.previous) {
			return driveStop{"execution-tool-sequence"}
		}
		return nil
	}
	if request.Sequence <= d.previous.Sequence {
		return driveStop{"execution-tool-sequence"}
	}
	runCtx, cancel := context.WithDeadline(ctx, time.UnixMilli(request.ExpiresAt).Add(-toolAnswerMargin))
	defer cancel()
	lost := d.watchAuthority(runCtx, cancel)
	status, text := 200, ""
	call, err := tektontasks.ParseToolCall(request.Payload)
	if err == nil {
		var result tektontasks.ToolResult
		result, err = d.run(runCtx, call)
		text = result.Text()
	}
	cancel()
	if lost() || ctx.Err() != nil {
		return driveStop{"execution-authority"}
	}
	if err != nil {
		status, text = toolFailure(err)
	}
	response := modelmailbox.Envelope{Version: modelmailbox.Version, Kind: "tool-response", Attempt: request.Attempt, PodUID: request.PodUID,
		Sequence: request.Sequence, RequestSHA256: request.RequestSHA256, ExpiresAt: request.ExpiresAt, Status: status, Payload: toolPayload(text)}
	if err := d.session.PublishToolResponse(ctx, response); err != nil && !errors.Is(err, podtransport.ErrToolNotPending) {
		return driveStop{"execution-tool-publish"}
	}
	// A call no longer pending already ended for the generator (504).
	d.previous = request
	d.previous.Payload = append([]byte(nil), request.Payload...)
	return nil
}

// watchAuthority rechecks authority while a tool runs and cancels the run
// when it is refused. Evidence that is only stale is tolerated here as in the
// drive loop's own wait; any other refusal stops. It reports whether
// authority was lost; the watcher ends with ctx.
func (d *toolDispatch) watchAuthority(ctx context.Context, cancel context.CancelFunc) func() bool {
	var lost atomic.Bool
	if d.authorize == nil {
		return lost.Load
	}
	go func() {
		ticker := time.NewTicker(toolAuthorizeEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				err := d.authorize(ctx)
				var stale interface{ EvidenceWait() time.Duration }
				if err != nil && ctx.Err() == nil && !(errors.As(err, &stale) && stale.EvidenceWait() > 0) {
					lost.Store(true)
					cancel()
					return
				}
			}
		}
	}()
	return lost.Load
}

// authorizeToolCall checks a call against the assignment's tool spec on the
// controller side: the in-sandbox tools server filters the same way, but the
// generator can reach the mailbox route directly. The tool must be declared,
// offered (generatortools.Offered) and called with a declared target.
func authorizeToolCall(spec *generatortools.Spec, call tektontasks.ToolCall) error {
	if spec == nil {
		return tektontasks.ErrRequest
	}
	for _, tool := range spec.Tools {
		if tool.Name != call.Tool {
			continue
		}
		if !generatortools.Offered(tool.Kind) || !slices.Contains(tool.Targets, call.Target) {
			return tektontasks.ErrRequest
		}
		return nil
	}
	return tektontasks.ErrRequest
}

// toolFailure maps a refused or failed call to a fixed status and message.
// It never forwards an underlying error, cluster detail or log.
func toolFailure(err error) (int, string) {
	switch {
	case errors.Is(err, tektontasks.ErrToolBusy):
		return 429, "Another tool call of this generation is still running. Tools run one at a time: wait for its result, then call again if needed."
	case errors.Is(err, tektontasks.ErrToolBudget):
		return 429, "The tool budget of this generation is exhausted; finish without further tool calls."
	case errors.Is(err, tektontasks.ErrRequest):
		return 403, "The tool call is outside the reviewed manifest scope (tool, target, packages or candidate change)."
	case errors.Is(err, tektontasks.ErrDeadline), errors.Is(err, context.DeadlineExceeded):
		return 504, "The tool run did not finish within its bound."
	}
	return 500, "The tool run failed before producing a result."
}

// toolPayload encodes text within the mailbox tool response bound.
func toolPayload(text string) []byte {
	for limit := len(text); ; limit -= 1024 {
		if limit < 0 {
			limit = 0
		}
		raw, _ := json.Marshal(map[string]string{"text": text[:limit]})
		if len(raw) <= modelmailbox.MaxToolResponseBytes || limit == 0 {
			return raw
		}
	}
}

// CIRuntimeClass is the sandbox class the CI namespace's admission requires
// for build-test, the only catalog task that runs candidate code.
const CIRuntimeClass = "ai-sdlc-offline"

// toolPoll is how often a running tool's PipelineRun is observed.
const toolPoll = 10 * time.Second

// generatorTools returns the drive loop's tool hook for a generation, or nil
// when the generation offers no tools (the profile has no tool budget or the
// assignment's manifest declares none). The manifest is the one the current
// policy binds, and its tool spec must be exactly the assignment's: a reload
// that changed the declared tools refuses rather than runs other tools.
func generatorTools(ctx context.Context, prepared Prepared, load LifecycleLoader, session toolSession, cluster func() (tektontasks.Cluster, error), journal tektontasks.ToolJournal, authorize func(context.Context) error) (func(context.Context) error, error) {
	assignmentRaw := prepared.Assignment()
	a, err := executor.ParseAssignment(assignmentRaw, workspacebundle.Digest(assignmentRaw), time.Now())
	if err != nil {
		return nil, ErrAuthority
	}
	p, err := executor.ParseProfile(prepared.Profile(), a.ProfileSHA256)
	if err != nil {
		return nil, ErrAuthority
	}
	if !executor.ToolsEnabled(p, a.Tools) {
		return nil, nil
	}
	inputs, _, err := load(ctx)
	if err != nil {
		return nil, ErrAuthority
	}
	m, ok := project.BoundOrNone(a.Repository, inputs.Policy.ProjectSHA256)
	declared, _ := json.Marshal(ToolSpecOf(m))
	assigned, _ := json.Marshal(a.Tools)
	if !ok || !bytes.Equal(declared, assigned) {
		return nil, ErrAuthority
	}
	if journal == nil {
		return nil, ErrAuthority
	}
	c, err := cluster()
	if err != nil {
		return nil, err
	}
	budget := &tektontasks.ToolBudget{MaxCalls: p.Tools.Calls, MaxSeconds: p.MaxSeconds}
	dispatch := &toolDispatch{session: session, authorize: authorize, run: func(ctx context.Context, call tektontasks.ToolCall) (tektontasks.ToolResult, error) {
		if err := authorizeToolCall(a.Tools, call); err != nil {
			return tektontasks.ToolResult{}, err
		}
		return tektontasks.InvokeTool(ctx, c, journal, budget, m, call, a.SourceSHA, CIRuntimeClass, toolPoll)
	}}
	return dispatch.poll, nil
}

// storeToolJournal implements tektontasks.ToolJournal over the store's
// tool_calls journal for one model allocation.
type storeToolJournal struct {
	db           *store.DB
	allocationID string
}

func newToolJournal(db *store.DB, allocationID string) tektontasks.ToolJournal {
	if db == nil || allocationID == "" {
		return nil
	}
	return storeToolJournal{db: db, allocationID: allocationID}
}

func (j storeToolJournal) Begin(ctx context.Context, requestDigest string) (int, error) {
	return j.db.BeginToolCall(ctx, j.allocationID, requestDigest)
}

func (j storeToolJournal) Created(ctx context.Context, sequence int, pipelineRun, configMap string) error {
	return j.db.RecordToolCallObjects(ctx, j.allocationID, sequence, pipelineRun, configMap)
}

func (j storeToolJournal) Finish(ctx context.Context, sequence int, r tektontasks.ToolRecord) error {
	return j.db.FinishToolCall(ctx, j.allocationID, sequence, store.ToolCallRecord{PipelineRun: r.PipelineRun, ConfigMap: r.ConfigMap, Revision: r.Revision,
		Tree: r.Tree, SummarySHA256: r.SummarySHA256, State: r.State})
}
