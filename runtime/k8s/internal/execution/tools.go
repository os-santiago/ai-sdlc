package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
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
// consumes its answer, any other repeated or skipped sequence stops the run.
type toolDispatch struct {
	session  toolSession
	run      ToolRunner
	previous modelmailbox.Envelope
}

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
	if request.Sequence != d.previous.Sequence+1 {
		return driveStop{"execution-tool-sequence"}
	}
	status, text := 200, ""
	call, err := tektontasks.ParseToolCall(request.Payload)
	if err == nil {
		var result tektontasks.ToolResult
		result, err = d.run(ctx, call)
		text = result.Text()
	}
	if err != nil {
		status, text = toolFailure(err)
	}
	response := modelmailbox.Envelope{Version: modelmailbox.Version, Kind: "tool-response", Attempt: request.Attempt, PodUID: request.PodUID,
		Sequence: request.Sequence, RequestSHA256: request.RequestSHA256, ExpiresAt: request.ExpiresAt, Status: status, Payload: toolPayload(text)}
	if ctx.Err() != nil {
		return driveStop{"execution-authority"}
	}
	if d.session.PublishToolResponse(ctx, response) != nil {
		return driveStop{"execution-tool-publish"}
	}
	d.previous = request
	d.previous.Payload = append([]byte(nil), request.Payload...)
	return nil
}

// toolFailure maps a refused or failed call to a fixed status and message.
// It never forwards an underlying error, cluster detail or log.
func toolFailure(err error) (int, string) {
	switch {
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
func generatorTools(ctx context.Context, prepared Prepared, load LifecycleLoader, session toolSession, cluster func() (tektontasks.Cluster, error), journal tektontasks.ToolJournal) (func(context.Context) error, error) {
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
	// Answer inside the mailbox's own window, so the generator receives a
	// bounded failure rather than a bare timeout.
	window := time.Duration(p.Tools.Seconds)*time.Second - 10*time.Second
	dispatch := &toolDispatch{session: session, run: func(ctx context.Context, call tektontasks.ToolCall) (tektontasks.ToolResult, error) {
		ctx, cancel := context.WithTimeout(ctx, window)
		defer cancel()
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

func (j storeToolJournal) Finish(ctx context.Context, sequence int, r tektontasks.ToolRecord) error {
	return j.db.FinishToolCall(ctx, j.allocationID, sequence, store.ToolCallRecord{PipelineRun: r.PipelineRun, Revision: r.Revision,
		Tree: r.Tree, SummarySHA256: r.SummarySHA256, State: r.State})
}
