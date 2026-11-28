// Package sourcefetch converts pinned GitHub source into bounded, untrusted
// workspace bundles. It never extracts host paths or grants execution authority.
package sourcefetch

import (
	"archive/tar"
	"bufio"
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

// MaxArchiveBytes bounds the compressed archive and MaxExpandedArchiveBytes
// its tar stream: the workspace content plus, per node, a 512-byte header and
// less than 512 bytes of padding, and 1 MiB for the global header, the end
// blocks and record padding. Neither is ever held in memory: the archive is
// decompressed and parsed as it arrives, keeping one file at a time besides
// the bundle being built.
const (
	MaxArchiveBytes         = 96 * 1024 * 1024
	MaxExpandedArchiveBytes = workspacebundle.MaxContentBytes + workspacebundle.MaxNodes*1024 + 1024*1024
)

var ErrSource = errors.New("pinned source unavailable or outside archive contract")

// countingReader counts bytes read, refuses more than limit, and keeps the
// last 1024 bytes, which the tar end-of-archive check needs.
type countingReader struct {
	r     io.Reader
	n     int64
	limit int64
	tail  [1024]byte
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.n > c.limit {
		return 0, ErrSource
	}
	if n >= len(c.tail) {
		copy(c.tail[:], p[n-len(c.tail):n])
	} else {
		copy(c.tail[:], c.tail[n:])
		copy(c.tail[len(c.tail)-n:], p[:n])
	}
	return n, err
}

// DecodeArchive consumes exactly one gzip member and one bounded tar archive.
// Git archive's global PAX commit comment must match the requested immutable
// SHA. This is a consistency binding, not an independent signature/provenance.
func DecodeArchive(raw []byte, sourceSHA string) (workspacebundle.Bundle, error) {
	if len(raw) == 0 || len(raw) > MaxArchiveBytes {
		return workspacebundle.Bundle{Version: workspacebundle.Version, SourceSHA: sourceSHA, Files: []workspacebundle.File{}}, ErrSource
	}
	return DecodeArchiveFrom(bytes.NewReader(raw), sourceSHA)
}

// DecodeArchiveFrom is DecodeArchive over a stream: the compressed bytes are
// counted against MaxArchiveBytes and the tar stream against
// MaxExpandedArchiveBytes as they are read, so neither is buffered whole.
func DecodeArchiveFrom(raw io.Reader, sourceSHA string) (workspacebundle.Bundle, error) {
	b := workspacebundle.Bundle{Version: workspacebundle.Version, SourceSHA: sourceSHA, Files: []workspacebundle.File{}}
	if raw == nil || !workspacebundle.ValidSourceSHA(sourceSHA) {
		return b, ErrSource
	}
	compressedCount := &countingReader{r: raw, limit: MaxArchiveBytes}
	// A ByteReader keeps gzip from reading past its member, so the bytes left
	// in compressed afterwards are exactly the trailing data.
	compressed := bufio.NewReader(compressedCount)
	gz, err := gzip.NewReader(compressed)
	if err != nil {
		return b, ErrSource
	}
	gz.Multistream(false)
	input := &countingReader{r: gz, limit: MaxExpandedArchiveBytes}
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
		beforeNext := input.n
		h, err := reader.Next()
		if err == io.EOF {
			consumed := input.n - beforeNext
			var zeros [1024]byte
			if consumed != int64(endPadding+1024) || input.n < 1024 || input.tail != zeros {
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
			data := make([]byte, h.Size)
			var extra [1]byte
			if _, err := io.ReadFull(reader, data); err != nil {
				return b, ErrSource
			}
			if n, err := reader.Read(extra[:]); n != 0 || err != io.EOF {
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
	// Reading to the end of the member also verifies the gzip checksum.
	var block [32 * 1024]byte
	for {
		n, err := input.Read(block[:])
		for _, v := range block[:n] {
			if v != 0 {
				return b, ErrSource
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return b, ErrSource
		}
	}
	if gz.Close() != nil {
		return b, ErrSource
	}
	if _, err := compressed.ReadByte(); err != io.EOF {
		return b, ErrSource
	}
	if !global || root == "" || b.Validate() != nil {
		return b, ErrSource
	}
	// Every consumer encodes the tree; refuse one whose encoding exceeds the
	// wire bound here, as a source refusal.
	if _, err := workspacebundle.EncodedSize(b); err != nil {
		return b, ErrSource
	}
	sort.Slice(b.Files, func(i, j int) bool { return b.Files[i].Path < b.Files[j].Path })
	return b, nil
}
