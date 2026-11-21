package repositorytests

import (
	"sort"

	delta "github.com/os-santiago/ai-sdlc/runtime/k8s/internal/candidate"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// Trees are the three source trees of one verification and the candidate
// delta in the form Decide reads. They are built only from the fetched base
// bundle and the stored candidate delta, never from a workspace or a model.
type Trees struct {
	Baseline  workspacebundle.Bundle
	Cross     workspacebundle.Bundle
	Candidate workspacebundle.Bundle
	Changes   []Change
}

// BuildTrees reconstructs the candidate exactly (delta.Apply checks both
// tree digests) and the cross tree: the base revision plus only the added or
// modified files the manifest classifies as tests. Deleted tests are not
// applied to the cross tree; Decide refuses such a candidate anyway.
func BuildTrees(source workspacebundle.Bundle, d delta.Delta, v project.Verification) (Trees, error) {
	if v.Validate() != nil {
		return Trees{}, ErrWorkload
	}
	result, err := delta.Apply(source, d)
	if err != nil {
		return Trees{}, ErrWorkload
	}
	files := map[string]workspacebundle.File{}
	for _, f := range source.Files {
		files[f.Path] = f
	}
	changes := make([]Change, 0, len(d.Changes))
	for _, c := range d.Changes {
		changes = append(changes, Change{Path: c.Path, Deleted: c.After == nil, Added: c.Before == nil})
		if c.After != nil && v.MatchesTest(c.Path) {
			f := *c.After
			f.Data = append([]byte{}, f.Data...)
			files[c.Path] = f
		}
	}
	cross := workspacebundle.Bundle{Version: workspacebundle.Version, SourceSHA: source.SourceSHA, Files: make([]workspacebundle.File, 0, len(files))}
	for _, f := range files {
		cross.Files = append(cross.Files, f)
	}
	sort.Slice(cross.Files, func(i, j int) bool { return cross.Files[i].Path < cross.Files[j].Path })
	if cross.Validate() != nil {
		return Trees{}, ErrWorkload
	}
	return Trees{Baseline: source, Cross: cross, Candidate: result, Changes: changes}, nil
}

// Tree selects the tree a run imports.
func (t Trees) Tree(run string) (workspacebundle.Bundle, bool) {
	switch run {
	case RunBaseline:
		return t.Baseline, true
	case RunCross:
		return t.Cross, true
	case RunCandidate:
		return t.Candidate, true
	}
	return workspacebundle.Bundle{}, false
}
