// Package sourcefetch converts pinned GitHub source into bounded, untrusted
// workspace bundles. It never extracts host paths or grants execution authority.
package sourcefetch

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

const MaxArchiveBytes = 32 * 1024 * 1024

var ErrSource = errors.New("pinned source unavailable or outside archive contract")

// DecodeArchive consumes exactly one gzip member and one bounded tar archive.
// Git archive's global PAX commit comment must match the requested immutable
// SHA. This is a consistency binding, not an independent signature/provenance.
func DecodeArchive(raw []byte, sourceSHA string) (workspacebundle.Bundle, error) {
	b := workspacebundle.Bundle{Version: workspacebundle.Version, SourceSHA: sourceSHA, Files: []workspacebundle.File{}}
	if len(raw) == 0 || len(raw) > MaxArchiveBytes || !workspacebundle.ValidSourceSHA(sourceSHA) {
		return b, ErrSource
	}
	compressed := bytes.NewReader(raw)
	gz, err := gzip.NewReader(compressed)
	if err != nil {
		return b, ErrSource
	}
	gz.Multistream(false)
	expanded, err := io.ReadAll(io.LimitReader(gz, MaxArchiveBytes+1))
	closeErr := gz.Close()
	if err != nil || closeErr != nil || len(expanded) > MaxArchiveBytes || compressed.Len() != 0 {
		return b, ErrSource
	}
	input := bytes.NewReader(expanded)
	reader := tar.NewReader(input)
	root := ""
	global := false
	total := int64(0)
	nodes := 0
	seen := map[string]bool{}
	directories := map[string]bool{}
	files := map[string]bool{}
	endPadding := 0
	for {
		beforeNext := input.Len()
		h, err := reader.Next()
		if err == io.EOF {
			consumed := beforeNext - input.Len()
			end := len(expanded) - input.Len()
			var zeros [1024]byte
			if consumed != endPadding+1024 || end < 1024 || !bytes.Equal(expanded[end-1024:end], zeros[:]) {
				return b, ErrSource
			}
			break
		}
		if err != nil {
			return b, ErrSource
		}
		if h.Typeflag == tar.TypeXGlobalHeader {
			if global || root != "" || len(h.PAXRecords) != 1 || h.PAXRecords["comment"] != sourceSHA || h.Size != 0 {
				return b, ErrSource
			}
			global = true
			continue
		}
		if !global || h.Linkname != "" || len(h.Xattrs) != 0 || h.Size < 0 || len(h.PAXRecords) != 0 {
			return b, ErrSource
		}
		endPadding = (512 - int(h.Size%512)) % 512
		nodes++
		if nodes > workspacebundle.MaxNodes {
			return b, ErrSource
		}
		name := h.Name
		if h.Typeflag == tar.TypeDir {
			name = strings.TrimSuffix(name, "/")
		}
		if name == "" || len(name) > workspacebundle.MaxPathBytes+256 || !utf8.ValidString(name) || strings.ContainsRune(name, utf8.RuneError) || strings.ContainsAny(name, "\\\x00\r\n\t") || path.Clean(name) != name || strings.HasPrefix(name, "/") {
			return b, ErrSource
		}
		for _, part := range strings.Split(name, "/") {
			if part == "" || part == "." || part == ".." || len(part) > 255 || strings.EqualFold(part, ".git") {
				return b, ErrSource
			}
			for _, r := range part {
				if r < 32 || r == 127 {
					return b, ErrSource
				}
			}
		}
		if seen[name] {
			return b, ErrSource
		}
		seen[name] = true
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if files[parent] {
				return b, ErrSource
			}
			directories[parent] = true
			if len(directories)+len(files) > workspacebundle.MaxNodes {
				return b, ErrSource
			}
		}
		if h.Typeflag == tar.TypeDir {
			directories[name] = true
		} else {
			if directories[name] {
				return b, ErrSource
			}
			files[name] = true
		}
		if len(directories)+len(files) > workspacebundle.MaxNodes {
			return b, ErrSource
		}
		if root == "" {
			if h.Typeflag != tar.TypeDir || strings.Contains(name, "/") || h.Size != 0 || h.Mode & ^int64(0777) != 0 || !strings.HasSuffix(name, "-"+sourceSHA[:7]) {
				return b, ErrSource
			}
			root = name
			continue
		}
		if !strings.HasPrefix(name, root+"/") {
			return b, ErrSource
		}
		relative := strings.TrimPrefix(name, root+"/")
		if len(relative) > workspacebundle.MaxPathBytes || len(strings.Split(relative, "/")) > workspacebundle.MaxDepth {
			return b, ErrSource
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if h.Size != 0 || h.Mode&^0777 != 0 {
				return b, ErrSource
			}
		case tar.TypeReg, tar.TypeRegA:
			if h.Mode != 0644 && h.Mode != 0664 && h.Mode != 0755 && h.Mode != 0775 || h.Size > workspacebundle.MaxFileBytes || len(b.Files) >= workspacebundle.MaxFiles {
				return b, ErrSource
			}
			total += h.Size
			if total > workspacebundle.MaxContentBytes {
				return b, ErrSource
			}
			data, err := io.ReadAll(io.LimitReader(reader, h.Size+1))
			if err != nil || int64(len(data)) != h.Size {
				return b, ErrSource
			}
			// Git tracks only regular/executable identity. GitHub archives use
			// group-writable 0664/0775; normalize to the workspace contract.
			mode := uint32(0644)
			if h.Mode&0111 != 0 {
				mode = 0755
			}
			b.Files = append(b.Files, workspacebundle.File{Path: relative, Mode: mode, Data: data})
		default:
			return b, ErrSource
		}
	}
	// tar.Reader permits block padding; only zero padding after its end marker
	// is accepted. Concatenated tar/gzip archives and trailing junk are denied.
	tail, _ := io.ReadAll(input)
	for _, v := range tail {
		if v != 0 {
			return b, ErrSource
		}
	}
	if !global || root == "" || b.Validate() != nil {
		return b, ErrSource
	}
	sort.Slice(b.Files, func(i, j int) bool { return b.Files[i].Path < b.Files[j].Path })
	return b, nil
}
