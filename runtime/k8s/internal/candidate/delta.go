// Package candidate represents bounded, unvalidated changes. A delta is data,
// never evidence that tests passed or permission to publish a change.
package candidate

import (
	"bytes"
	"encoding/json"
	"errors"
	"sort"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

const Version = "ai-sdlc.candidate-delta/v1"
const MaxChanges = 64
const MaxBytes = 2 * 1024 * 1024

var ErrInvalid = errors.New("invalid or oversized unvalidated candidate delta")

// StepBudgetExitCode is the executor launcher's exit code for a generation
// that SCC ended on its tool-step budget (SCC's exit 22, BUDGET_EXCEEDED),
// established by the launcher's own clock rather than by SCC's output. Such a
// run's workspace is exported like a clean exit when it holds a change.
const StepBudgetExitCode = 25

// GenerationEndedStepBudget is the bounded marker a captured candidate carries
// when its generation ended on the step budget. It is evaluation data only:
// verification, review, publication and merge never read it, and it grants
// nothing.
const GenerationEndedStepBudget = "step-budget"

type Identity struct {
	Mode   uint32 `json:"mode"`
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
}
type Change struct {
	Path   string                `json:"path"`
	Before *Identity             `json:"before"`
	After  *workspacebundle.File `json:"after"`
}
type Delta struct {
	Version             string   `json:"version"`
	SourceSHA           string   `json:"sourceSHA"`
	SourceTreeSHA256    string   `json:"sourceTreeSHA256"`
	CandidateTreeSHA256 string   `json:"candidateTreeSHA256"`
	Changes             []Change `json:"changes"`
}

func identity(f workspacebundle.File) Identity {
	return Identity{f.Mode, len(f.Data), workspacebundle.Digest(f.Data)}
}
func clone(f workspacebundle.File) workspacebundle.File {
	f.Data = append([]byte{}, f.Data...)
	return f
}
func (d Delta) valid() bool {
	if d.Version != Version || !workspacebundle.ValidSourceSHA(d.SourceSHA) ||
		!workspacebundle.ValidDigest(d.SourceTreeSHA256) || !workspacebundle.ValidDigest(d.CandidateTreeSHA256) ||
		d.SourceTreeSHA256 == d.CandidateTreeSHA256 || len(d.Changes) < 1 || len(d.Changes) > MaxChanges {
		return false
	}
	previous := ""
	for _, change := range d.Changes {
		if !workspacebundle.ValidPath(change.Path) || change.Path <= previous || (change.Before == nil && change.After == nil) {
			return false
		}
		previous = change.Path
		if change.Before != nil && ((change.Before.Mode != 0644 && change.Before.Mode != 0755) || change.Before.Bytes < 0 || change.Before.Bytes > workspacebundle.MaxFileBytes || !workspacebundle.ValidDigest(change.Before.SHA256)) {
			return false
		}
		if change.After != nil {
			if change.After.Path != change.Path || (workspacebundle.Bundle{Version: workspacebundle.Version, SourceSHA: d.SourceSHA, Files: []workspacebundle.File{*change.After}}).Validate() != nil {
				return false
			}
			if change.Before != nil && *change.Before == identity(*change.After) {
				return false
			}
		}
	}
	return true
}

func Encode(d Delta) ([]byte, error) {
	if !d.valid() {
		return nil, ErrInvalid
	}
	raw, err := json.Marshal(d)
	if err != nil || len(raw) > MaxBytes {
		return nil, ErrInvalid
	}
	return raw, nil
}
func Decode(raw []byte) (Delta, error) {
	var d Delta
	if len(raw) == 0 || len(raw) > MaxBytes || authority.DecodeStrict(raw, &d) != nil || !d.valid() {
		return Delta{}, ErrInvalid
	}
	// Persist one canonical representation, including sorted changes and exact
	// JSON field shape. Duplicate keys/unknown fields were already rejected.
	canonical, err := Encode(d)
	if err != nil || !bytes.Equal(canonical, raw) {
		return Delta{}, ErrInvalid
	}
	return d, nil
}

func Build(source, result workspacebundle.Bundle) (Delta, error) {
	if source.Validate() != nil || result.Validate() != nil || source.SourceSHA != result.SourceSHA {
		return Delta{}, ErrInvalid
	}
	d := Delta{Version: Version, SourceSHA: source.SourceSHA}
	d.SourceTreeSHA256, _ = source.TreeDigest()
	d.CandidateTreeSHA256, _ = result.TreeDigest()
	before, after := map[string]workspacebundle.File{}, map[string]workspacebundle.File{}
	paths := map[string]bool{}
	for _, f := range source.Files {
		before[f.Path] = f
		paths[f.Path] = true
	}
	for _, f := range result.Files {
		after[f.Path] = f
		paths[f.Path] = true
	}
	ordered := make([]string, 0, len(paths))
	for path := range paths {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	for _, path := range ordered {
		a, aok := before[path]
		b, bok := after[path]
		if aok && bok && a.Mode == b.Mode && bytes.Equal(a.Data, b.Data) {
			continue
		}
		change := Change{Path: path}
		if aok {
			id := identity(a)
			change.Before = &id
		}
		if bok {
			copied := clone(b)
			change.After = &copied
		}
		d.Changes = append(d.Changes, change)
		if len(d.Changes) > MaxChanges {
			return Delta{}, ErrInvalid
		}
	}
	if _, err := Encode(d); err != nil {
		return Delta{}, ErrInvalid
	}
	return d, nil
}

// Apply independently reconstructs every file and mode from the exact base.
// It never reads a filesystem or runs repository-supplied commands.
func Apply(source workspacebundle.Bundle, d Delta) (workspacebundle.Bundle, error) {
	if source.Validate() != nil || source.SourceSHA != d.SourceSHA {
		return workspacebundle.Bundle{}, ErrInvalid
	}
	if _, err := Encode(d); err != nil {
		return workspacebundle.Bundle{}, ErrInvalid
	}
	tree, _ := source.TreeDigest()
	if tree != d.SourceTreeSHA256 {
		return workspacebundle.Bundle{}, ErrInvalid
	}
	files := map[string]workspacebundle.File{}
	for _, f := range source.Files {
		files[f.Path] = clone(f)
	}
	for _, change := range d.Changes {
		old, exists := files[change.Path]
		if (change.Before == nil && exists) || (change.Before != nil && (!exists || *change.Before != identity(old))) {
			return workspacebundle.Bundle{}, ErrInvalid
		}
		if change.After == nil {
			delete(files, change.Path)
		} else {
			files[change.Path] = clone(*change.After)
		}
	}
	result := workspacebundle.Bundle{Version: workspacebundle.Version, SourceSHA: source.SourceSHA, Files: []workspacebundle.File{}}
	for _, f := range files {
		result.Files = append(result.Files, f)
	}
	sort.Slice(result.Files, func(i, j int) bool { return result.Files[i].Path < result.Files[j].Path })
	tree, err := result.TreeDigest()
	if err != nil || tree != d.CandidateTreeSHA256 {
		return workspacebundle.Bundle{}, ErrInvalid
	}
	return result, nil
}
