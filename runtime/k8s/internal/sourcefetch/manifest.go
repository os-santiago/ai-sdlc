package sourcefetch

import (
	"bytes"
	"context"
	"crypto/sha1" // Git's existing object-ID format, not a signature or attestation.
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

const MaxManifestBytes = 8 * 1024 * 1024

type treeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
	Size *int64 `json:"size,omitempty"`
	URL  string `json:"url"`
}
type treeManifest struct {
	SHA       string      `json:"sha"`
	URL       string      `json:"url"`
	Tree      []treeEntry `json:"tree"`
	Truncated *bool       `json:"truncated"`
}

func (c *Client) getJSON(ctx context.Context, suffix string, limit int) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.api+"/repos/"+c.repository+suffix, nil)
	if err != nil {
		return nil, ErrSource
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	response, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() == nil && networkFailure(err) {
			return nil, errNetwork
		}
		return nil, ErrSource
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, c.responseError(response)
	}
	if response.Header.Get("Content-Encoding") != "" || response.ContentLength > int64(limit) {
		return nil, ErrSource
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, int64(limit)+1))
	if err != nil || len(raw) == 0 || len(raw) > limit || !utf8.Valid(raw) || ctx.Err() != nil {
		return nil, ErrSource
	}
	return raw, nil
}

func (c *Client) manifest(ctx context.Context, commitSHA string) (treeManifest, error) {
	raw, err := c.getJSON(ctx, "/git/commits/"+commitSHA, 1024*1024)
	if err != nil {
		return treeManifest{}, err
	}
	// GitHub owns commit resolution. Ignore unrelated commit metadata, while
	// rejecting duplicate/case-conflicting JSON keys throughout the response.
	var commit map[string]json.RawMessage
	if authority.DecodeStrict(raw, &commit) != nil {
		return treeManifest{}, ErrSource
	}
	var observed string
	var tree map[string]json.RawMessage
	var root string
	if json.Unmarshal(commit["sha"], &observed) != nil || observed != commitSHA || json.Unmarshal(commit["tree"], &tree) != nil || json.Unmarshal(tree["sha"], &root) != nil || !workspacebundle.ValidSourceSHA(root) {
		return treeManifest{}, ErrSource
	}
	raw, err = c.getJSON(ctx, "/git/trees/"+root+"?recursive=1", MaxManifestBytes)
	if err != nil {
		return treeManifest{}, err
	}
	var m treeManifest
	if authority.DecodeStrict(raw, &m) != nil || m.SHA != root || m.Truncated == nil || *m.Truncated || m.Tree == nil || len(m.Tree) > workspacebundle.MaxNodes {
		return treeManifest{}, ErrSource
	}
	if _, err = m.entries(); err != nil {
		return treeManifest{}, err
	}
	return m, nil
}

func (m treeManifest) entries() (map[string]treeEntry, error) {
	entries := map[string]treeEntry{}
	total := int64(0)
	files := 0
	for _, e := range m.Tree {
		if !workspacebundle.ValidPath(e.Path) || !workspacebundle.ValidSourceSHA(e.SHA) {
			return nil, ErrSource
		}
		if _, exists := entries[e.Path]; exists {
			return nil, ErrSource
		}
		switch {
		case e.Type == "tree" && e.Mode == "040000":
			if e.Size != nil {
				return nil, ErrSource
			}
		case e.Type == "blob" && (e.Mode == "100644" || e.Mode == "100755"):
			if e.Size == nil || *e.Size < 0 || *e.Size > workspacebundle.MaxFileBytes {
				return nil, ErrSource
			}
			files++
			total += *e.Size
			if files > workspacebundle.MaxFiles || total > workspacebundle.MaxContentBytes {
				return nil, ErrSource
			}
		default:
			return nil, ErrSource // Includes symlinks and submodule commits.
		}
		entries[e.Path] = e
	}
	for name := range entries {
		parent := path.Dir(name)
		if parent != "." {
			if e, ok := entries[parent]; !ok || e.Type != "tree" {
				return nil, ErrSource
			}
		}
	}
	return entries, nil
}

func gitObject(kind string, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", kind, len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func (m treeManifest) verify(b workspacebundle.Bundle) error {
	if b.Validate() != nil || !workspacebundle.ValidSourceSHA(m.SHA) || m.Truncated == nil || *m.Truncated || m.Tree == nil || len(m.Tree) > workspacebundle.MaxNodes {
		return ErrSource
	}
	entries, err := m.entries()
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, f := range b.Files {
		e, ok := entries[f.Path]
		mode := "100644"
		if f.Mode == 0755 {
			mode = "100755"
		}
		if !ok || e.Type != "blob" || e.Mode != mode || int64(len(f.Data)) != *e.Size || gitObject("blob", f.Data) != e.SHA {
			return ErrSource
		}
		seen[f.Path] = true
	}
	children := map[string][]treeEntry{}
	directories := []string{"."}
	for name, e := range entries {
		if e.Type == "blob" && !seen[name] {
			return ErrSource
		}
		parent := path.Dir(name)
		children[parent] = append(children[parent], e)
		if e.Type == "tree" {
			directories = append(directories, name)
		}
	}
	// Verify every directory object, including the root reported by the pinned
	// commit API. Git sorts a directory as basename+'/' against regular files.
	sort.Slice(directories, func(i, j int) bool { return len(directories[i]) > len(directories[j]) })
	for _, directory := range directories {
		list := children[directory]
		key := func(e treeEntry) string {
			k := path.Base(e.Path)
			if e.Type == "tree" {
				k += "/"
			}
			return k
		}
		sort.Slice(list, func(i, j int) bool { return key(list[i]) < key(list[j]) })
		var raw bytes.Buffer
		for _, e := range list {
			mode := strings.TrimLeft(e.Mode, "0")
			fmt.Fprintf(&raw, "%s %s\x00", mode, path.Base(e.Path))
			hash, _ := hex.DecodeString(e.SHA)
			raw.Write(hash)
		}
		expected := m.SHA
		if directory != "." {
			expected = entries[directory].SHA
		}
		if gitObject("tree", raw.Bytes()) != expected {
			return ErrSource
		}
	}
	return nil
}
