// Package workspacebundle transports bounded, untrusted file trees. It grants
// no execution, provenance, publishing or provider authority.
package workspacebundle

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	Version         = "ai-sdlc.workspace-bundle/v1"
	MaxWireBytes    = 32 * 1024 * 1024
	MaxContentBytes = 16 * 1024 * 1024
	MaxFileBytes    = 4 * 1024 * 1024
	MaxFiles        = 4096
	MaxNodes        = 8192
	MaxPathBytes    = 1024
	MaxDepth        = 32
)

var ErrInvalid = errors.New("invalid or unsafe workspace bundle")
var shaPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)
var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type File struct {
	Path string `json:"path"`
	Mode uint32 `json:"mode"`
	Data []byte `json:"data"`
}
type Bundle struct {
	Version   string `json:"version"`
	SourceSHA string `json:"sourceSHA"`
	Files     []File `json:"files"`
}

func Digest(raw []byte) string       { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }
func ValidSourceSHA(sha string) bool { return shaPattern.MatchString(sha) }
func ValidDigest(sha string) bool    { return digestPattern.MatchString(sha) }

// ValidPath reports whether a repository-relative path fits the bundle contract.
func ValidPath(name string) bool {
	if name == "" || len(name) > MaxPathBytes || !utf8.ValidString(name) || strings.ContainsRune(name, utf8.RuneError) || strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\\\x00\r\n\t") || path.Clean(name) != name {
		return false
	}
	parts := strings.Split(name, "/")
	if len(parts) > MaxDepth {
		return false
	}
	for _, part := range parts {
		if part == "" || len(part) > 255 || part == "." || part == ".." || strings.EqualFold(part, ".git") {
			return false
		}
		for _, r := range part {
			if r < 32 || r == 127 {
				return false
			}
		}
	}
	return true
}
func (b Bundle) Validate() error {
	if b.Version != Version || !ValidSourceSHA(b.SourceSHA) || b.Files == nil || len(b.Files) > MaxFiles {
		return ErrInvalid
	}
	names := map[string]bool{}
	total := 0
	for _, f := range b.Files {
		if !ValidPath(f.Path) || names[f.Path] || (f.Mode != 0644 && f.Mode != 0755) || f.Data == nil || len(f.Data) > MaxFileBytes {
			return ErrInvalid
		}
		total += len(f.Data)
		if total > MaxContentBytes {
			return ErrInvalid
		}
		names[f.Path] = true
	}
	nodes := len(names)
	directories := map[string]bool{}
	for name := range names {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if names[parent] {
				return ErrInvalid
			}
			if !directories[parent] {
				directories[parent] = true
				nodes++
				if nodes > MaxNodes {
					return ErrInvalid
				}
			}
		}
	}
	return nil
}
func readString(d *json.Decoder) (string, error) {
	t, e := d.Token()
	v, ok := t.(string)
	if e != nil || !ok {
		return "", ErrInvalid
	}
	return v, nil
}
func delim(d *json.Decoder, want json.Delim) error {
	v, e := d.Token()
	if e != nil || v != want {
		return ErrInvalid
	}
	return nil
}
func decodeFile(d *json.Decoder) (File, error) {
	var f File
	if delim(d, '{') != nil {
		return f, ErrInvalid
	}
	seen := map[string]bool{}
	for d.More() {
		key, e := readString(d)
		if e != nil || seen[key] {
			return f, ErrInvalid
		}
		seen[key] = true
		switch key {
		case "path":
			f.Path, e = readString(d)
			if e != nil || !ValidPath(f.Path) {
				return f, ErrInvalid
			}
		case "mode":
			v, e := d.Token()
			if e != nil {
				return f, ErrInvalid
			}
			n, ok := v.(json.Number)
			if !ok {
				return f, ErrInvalid
			}
			mode, e := n.Int64()
			if e != nil || (mode != 0644 && mode != 0755) {
				return f, ErrInvalid
			}
			f.Mode = uint32(mode)
		case "data":
			v, e := readString(d)
			if e != nil || len(v) > base64.StdEncoding.EncodedLen(MaxFileBytes) {
				return f, ErrInvalid
			}
			f.Data, e = base64.StdEncoding.Strict().DecodeString(v)
			if e != nil || base64.StdEncoding.EncodeToString(f.Data) != v || len(f.Data) > MaxFileBytes {
				return f, ErrInvalid
			}
		default:
			return f, ErrInvalid
		}
	}
	if len(seen) != 3 || delim(d, '}') != nil {
		return f, ErrInvalid
	}
	return f, nil
}

// Decode counts entries and decoded bytes while parsing. Base64 is size-checked
// before allocating its decoded body. Keys are exact and unique at every level.
func Decode(raw []byte) (Bundle, error) {
	var b Bundle
	if len(raw) == 0 || len(raw) > MaxWireBytes || !utf8.Valid(raw) {
		return b, ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if delim(d, '{') != nil {
		return b, ErrInvalid
	}
	seen := map[string]bool{}
	total := 0
	for d.More() {
		key, e := readString(d)
		if e != nil || seen[key] {
			return b, ErrInvalid
		}
		seen[key] = true
		switch key {
		case "version":
			b.Version, e = readString(d)
			if e != nil || b.Version != Version {
				return b, ErrInvalid
			}
		case "sourceSHA":
			b.SourceSHA, e = readString(d)
			if e != nil || !ValidSourceSHA(b.SourceSHA) {
				return b, ErrInvalid
			}
		case "files":
			if delim(d, '[') != nil {
				return b, ErrInvalid
			}
			b.Files = make([]File, 0)
			for d.More() {
				if len(b.Files) >= MaxFiles {
					return b, ErrInvalid
				}
				f, e := decodeFile(d)
				if e != nil {
					return b, ErrInvalid
				}
				total += len(f.Data)
				if total > MaxContentBytes {
					return b, ErrInvalid
				}
				b.Files = append(b.Files, f)
			}
			if delim(d, ']') != nil {
				return b, ErrInvalid
			}
		default:
			return b, ErrInvalid
		}
	}
	if len(seen) != 3 || delim(d, '}') != nil {
		return b, ErrInvalid
	}
	if _, e := d.Token(); e != io.EOF {
		return b, ErrInvalid
	}
	return b, b.Validate()
}
func Encode(b Bundle) ([]byte, error) {
	if b.Validate() != nil {
		return nil, ErrInvalid
	}
	raw, e := json.Marshal(b)
	if e != nil || len(raw) > MaxWireBytes {
		return nil, ErrInvalid
	}
	return raw, nil
}
func ReadVerified(input io.Reader, expectedDigest, sourceSHA string) (Bundle, error) {
	if !ValidDigest(expectedDigest) || !ValidSourceSHA(sourceSHA) {
		return Bundle{}, ErrInvalid
	}
	raw, e := io.ReadAll(io.LimitReader(input, MaxWireBytes+1))
	if e != nil || len(raw) > MaxWireBytes || Digest(raw) != expectedDigest {
		return Bundle{}, ErrInvalid
	}
	b, e := Decode(raw)
	if e != nil || b.SourceSHA != sourceSHA {
		return Bundle{}, ErrInvalid
	}
	return b, nil
}

// TreeDigest binds a validated file tree independently of JSON formatting or
// input order. The wire encoding is versioned and domain-separated: prefix,
// 20-byte source commit, u32 file count, then UTF-8-byte-sorted entries with u32
// path length, path bytes, u32 Git mode, u64 data length and 32-byte content hash.
// Integers are unsigned big-endian. No source file or caller slice is mutated.
func (b Bundle) TreeDigest() (string, error) {
	if b.Validate() != nil {
		return "", ErrInvalid
	}
	files := append([]File(nil), b.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	h := sha256.New()
	h.Write([]byte("ai-sdlc.workspace-tree/v1\x00"))
	source, _ := hex.DecodeString(b.SourceSHA)
	h.Write(source)
	var u32 [4]byte
	var u64 [8]byte
	binary.BigEndian.PutUint32(u32[:], uint32(len(files)))
	h.Write(u32[:])
	for _, f := range files {
		binary.BigEndian.PutUint32(u32[:], uint32(len(f.Path)))
		h.Write(u32[:])
		h.Write([]byte(f.Path))
		binary.BigEndian.PutUint32(u32[:], f.Mode)
		h.Write(u32[:])
		binary.BigEndian.PutUint64(u64[:], uint64(len(f.Data)))
		h.Write(u64[:])
		sum := sha256.Sum256(f.Data)
		h.Write(sum[:])
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
