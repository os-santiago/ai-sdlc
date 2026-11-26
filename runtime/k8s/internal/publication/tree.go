package publication

import (
	"bytes"
	"crypto/sha1" // Git object identities; SHA-256 remains the evidence binding.
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/candidate"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verification"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

type TreeEntry struct {
	Path    string `json:"path"`
	Mode    string `json:"mode"`
	Type    string `json:"type"`
	Content string `json:"content"`
}
type TreeRequest struct {
	BaseTree string      `json:"base_tree"`
	Tree     []TreeEntry `json:"tree"`
}
type TreePlan struct {
	Repository      string
	Request         TreeRequest
	ExpectedTreeSHA string
	Branch          string
	CommitMessage   string
	// CommitIdentity is resolved from the installed manifest when the plan
	// is prepared. The plan digest the claim records binds it, so the
	// commit a publication creates keeps the identity it was planned with
	// even if the installation is reconfigured while it is in flight.
	CommitIdentity project.CommitIdentity
}

// PrepareTree is pure reconstruction, not a publication authorization. It must
// be followed by current signed evidence checks and a durable effect journal.
// Only the finite files in the repository-specific reviewed recipe can be written.
// projectSHA256 is the ProjectSHA256 of the policy that authorized c (the
// policy whose Revision is c.AuthorityRevision). The plan uses only the
// manifest that policy binds, never a manifest from another policy load.
func PrepareTree(c Command, projectSHA256 string, source workspacebundle.Bundle, delta []byte) (TreePlan, error) {
	if !project.Supported(c.Repository) || c.SourceSHA != source.SourceSHA || c.IssueNumber < 1 || !candidateID.MatchString(c.CandidateID) || workspacebundle.Digest(delta) != c.DeltaSHA256 {
		return TreePlan{}, ErrPublication
	}
	manifest, ok := project.Bound(c.Repository, projectSHA256)
	if !ok {
		return TreePlan{}, ErrPublication
	}
	if len(manifest.Recipes) > 0 {
		// Reference repositories re-check their reviewed recipe's exact file set.
		if _, err := verification.Prepare(c.Repository, verification.RecipeForRepository(c.Repository), "AI-SDLC validation recipe: "+verification.RecipeForRepository(c.Repository), source, delta); err != nil {
			return TreePlan{}, ErrPublication
		}
	}
	d, err := candidate.Decode(delta)
	if err != nil || d.CandidateTreeSHA256 != c.CandidateTreeSHA256 || len(d.Changes) == 0 || len(d.Changes) > manifest.Publication.MaxChangedFiles {
		return TreePlan{}, ErrPublication
	}
	for _, change := range d.Changes {
		// Publications only write regular files the manifest allows.
		if !manifest.Publication.Allows(change.Path) || change.After == nil || change.After.Mode != 0644 {
			return TreePlan{}, ErrPublication
		}
	}
	result, err := candidate.Apply(source, d)
	if err != nil {
		return TreePlan{}, ErrPublication
	}
	base, err := GitTreeSHA(source)
	if err != nil {
		return TreePlan{}, err
	}
	expected, err := GitTreeSHA(result)
	if err != nil {
		return TreePlan{}, err
	}
	entries := make([]TreeEntry, 0, 2)
	for _, change := range d.Changes {
		if change.After == nil || !utf8.Valid(change.After.Data) {
			return TreePlan{}, ErrPublication
		}
		entries = append(entries, TreeEntry{Path: change.Path, Mode: "100644", Type: "blob", Content: string(change.After.Data)})
	}
	// Keep the tested public VPS release workflow out of the local-kind pilot.
	// No model or issue text can remove the explicit release-skip marker.
	return TreePlan{Repository: c.Repository, Request: TreeRequest{base, entries}, ExpectedTreeSHA: expected,
		Branch: BranchName(c), CommitMessage: CommitMessage(c.IssueNumber), CommitIdentity: manifest.Publication.Identity()}, nil
}

// BranchName is the only branch a command may publish to. It is derived from
// the signed issue number and candidate identity, never from model output.
func BranchName(c Command) string {
	if !candidateID.MatchString(c.CandidateID) || c.IssueNumber < 1 {
		return ""
	}
	return "ai-sdlc/issue-" + strconv.Itoa(c.IssueNumber) + "-" + strings.TrimPrefix(c.CandidateID, "execution-")[:32]
}

// CommitMessage is the fixed commit subject (and pull request title).
func CommitMessage(issue int) string {
	return fmt.Sprintf("fix(ai-sdlc): address issue #%d [skip release]", issue)
}

// PlanDigest binds a command to its reconstructed tree plan. The release
// manager journals it when it claims a publication.
func PlanDigest(c Command, p TreePlan) string {
	raw, _ := json.Marshal(struct {
		Command Command
		Plan    TreePlan
	}{c, p})
	return authority.Digest(raw)
}

type gitNode struct {
	children map[string]*gitNode
	file     *workspacebundle.File
}

func objectID(kind string, raw []byte) []byte {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", kind, len(raw))
	h.Write(raw)
	return h.Sum(nil)
}
func hashNode(node *gitNode) []byte {
	keys := make([]string, 0, len(node.children))
	for name, child := range node.children {
		key := name
		if child.file == nil {
			key += "/"
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var raw bytes.Buffer
	for _, key := range keys {
		name := strings.TrimSuffix(key, "/")
		child := node.children[name]
		mode := "40000"
		var digest []byte
		if child.file != nil {
			mode = "100644"
			if child.file.Mode == 0755 {
				mode = "100755"
			}
			digest = objectID("blob", child.file.Data)
		} else {
			digest = hashNode(child)
		}
		fmt.Fprintf(&raw, "%s %s\x00", mode, name)
		raw.Write(digest)
	}
	return objectID("tree", raw.Bytes())
}

// GitTreeSHA independently reconstructs canonical Git object identities without
// invoking git, filters, attributes, hooks or code from the source repository.
func GitTreeSHA(bundle workspacebundle.Bundle) (string, error) {
	if bundle.Validate() != nil {
		return "", ErrPublication
	}
	root := &gitNode{children: map[string]*gitNode{}}
	for _, file := range bundle.Files {
		node := root
		parts := strings.Split(file.Path, "/")
		for i, part := range parts {
			child := node.children[part]
			if child == nil {
				child = &gitNode{children: map[string]*gitNode{}}
				node.children[part] = child
			}
			if i == len(parts)-1 {
				copied := file
				child.file = &copied
			}
			node = child
		}
	}
	return hex.EncodeToString(hashNode(root)), nil
}
