package podtransport

import (
	"bytes"
	"context"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// Candidate is an untrusted artifact collected after the bound writer exited.
// Neither an exit code nor a valid bundle establishes acceptance or authority.
type Candidate struct {
	raw      []byte
	exitCode int32
}

func (c Candidate) Bytes() []byte         { return append([]byte(nil), c.raw...) }
func (c Candidate) WorkerExitCode() int32 { return c.exitCode }

// WorkerExit observes only the originally bound worker instance. It invokes no
// helper and does not make a candidate trustworthy or grant execution authority.
func (s *Session) WorkerExit(ctx context.Context) (code int32, exited bool, err error) {
	if s == nil {
		return 0, false, ErrTransport
	}
	observed, err := s.inspect(ctx, true)
	if err != nil {
		return 0, false, err
	}
	if observed.workerExitCode == nil {
		return 0, false, nil
	}
	return *observed.workerExitCode, true, nil
}

// WorkerFailure is an observation of the original non-restarted container only.
// It never invokes a helper, exports workspace data or authorizes another run.
func (s *Session) WorkerFailure(ctx context.Context) (podUID, containerID string, code int32, err error) {
	if s == nil {
		return "", "", 0, ErrTransport
	}
	o, err := s.inspect(ctx, true)
	if err != nil || o.workerExitCode == nil || *o.workerExitCode < 1 || *o.workerExitCode > 255 || o.workerTerminalID != o.instances["worker"].ID {
		return "", "", 0, ErrTransport
	}
	return s.identity.PodUID, o.instances["worker"].ID, *o.workerExitCode, nil
}

// ImportSource accepts only the exact source/tree bound by the assignment.
// It does not renew execution authority or authorize a later start. An uncertain
// import must be reconciled by a durable controller, never automatically retried.
func (s *Session) ImportSource(ctx context.Context, source []byte) error {
	if s == nil || len(source) == 0 || len(source) > workspacebundle.MaxWireBytes {
		return ErrTransport
	}
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	source = append([]byte(nil), source...)
	digest := workspacebundle.Digest(source)
	bundle, err := workspacebundle.ReadVerified(bytes.NewReader(source), digest, s.sourceSHA)
	if err != nil {
		return ErrTransport
	}
	tree, err := bundle.TreeDigest()
	if err != nil || tree != s.treeSHA {
		return ErrTransport
	}
	if _, err = s.observe(ctx, false); err != nil {
		return err
	}
	raw, code, err := s.exchangeFixed(ctx, helperOperation{
		command:    []string{"/workspace-transfer", "import", "--pod-uid", s.identity.PodUID, "--source-sha", s.sourceSHA, "--digest", digest},
		inputLimit: workspacebundle.MaxWireBytes, outputLimit: 2048, frames: 1024, timeout: 30 * time.Second, stdin: true,
	}, source)
	if err != nil {
		return err
	}
	if code != 0 {
		return ErrTransport
	}
	var receipt struct {
		Imported     *bool   `json:"imported"`
		Files        *int    `json:"files"`
		SourceSHA    *string `json:"sourceSHA"`
		BundleSHA256 *string `json:"bundleSHA256"`
	}
	if authority.DecodeStrict(raw, &receipt) != nil || receipt.Imported == nil || !*receipt.Imported || receipt.Files == nil || *receipt.Files != len(bundle.Files) || receipt.SourceSHA == nil || *receipt.SourceSHA != s.sourceSHA || receipt.BundleSHA256 == nil || *receipt.BundleSHA256 != digest {
		return ErrTransport
	}
	_, err = s.observe(ctx, false)
	return err
}

// Start publishes the exclusive fixed marker. The launcher independently checks
// source identity before executing SCC. Current signed authority and durable
// ownership remain caller duties, including on failure or lost acknowledgement.
func (s *Session) Start(ctx context.Context) error {
	if s == nil {
		return ErrTransport
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, err := s.observe(ctx, false); err != nil {
		return err
	}
	raw, code, err := s.exchangeFixed(ctx, helperOperation{
		command:     []string{"/executor-control", "start", s.identity.PodUID, s.assignmentDigest, s.treeSHA},
		outputLimit: 0, frames: 1024, timeout: 5 * time.Second,
	}, nil)
	if err != nil {
		return err
	}
	if code != 0 || len(raw) != 0 {
		return ErrTransport
	}
	_, err = s.observe(ctx, true)
	return err
}

// ExportCandidate requires the same terminated worker before and after the
// bounded export. It accepts nonzero exits for diagnostic inspection; callers
// must independently evaluate exit status, content, tests and policy.
func (s *Session) ExportCandidate(ctx context.Context) (Candidate, error) {
	if s == nil {
		return Candidate{}, ErrTransport
	}
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	before, err := s.inspect(ctx, true)
	if err != nil {
		return Candidate{}, err
	}
	if before.workerExitCode == nil {
		return Candidate{}, ErrTransport
	}
	raw, code, err := s.exchangeFixed(ctx, helperOperation{
		command:     []string{"/workspace-transfer", "export", "--pod-uid", s.identity.PodUID, "--source-sha", s.sourceSHA},
		outputLimit: workspacebundle.MaxWireBytes, frames: 8192, timeout: 30 * time.Second,
	}, nil)
	if err != nil {
		return Candidate{}, err
	}
	if code != 0 {
		return Candidate{}, ErrTransport
	}
	bundle, err := workspacebundle.Decode(raw)
	if err != nil || bundle.SourceSHA != s.sourceSHA {
		return Candidate{}, ErrTransport
	}
	after, err := s.inspect(ctx, true)
	if err != nil {
		return Candidate{}, err
	}
	if after.workerExitCode == nil || *after.workerExitCode != *before.workerExitCode {
		return Candidate{}, ErrTransport
	}
	return Candidate{raw: raw, exitCode: *after.workerExitCode}, nil
}
