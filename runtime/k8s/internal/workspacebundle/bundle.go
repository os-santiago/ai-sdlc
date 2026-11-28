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
	"hash"
	"io"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	Version = "ai-sdlc.workspace-bundle/v1"
	// MaxWireBytes leaves room for base64 of MaxContentBytes (4/3) plus about
	// 1.3 KiB of JSON per file at MaxFiles.
	MaxWireBytes    = 192 * 1024 * 1024
	MaxContentBytes = 128 * 1024 * 1024
	MaxFileBytes    = 4 * 1024 * 1024
	MaxFiles        = 16384
	MaxNodes        = 32768
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
	names := make(map[string]bool, len(b.Files))
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
	return checkTree(names)
}

// checkTree counts files plus distinct implicit directories against MaxNodes
// and refuses a file that is also another file's parent directory.
func checkTree(names map[string]bool) error {
	nodes := len(names)
	directories := map[string]bool{}
	for name := range names {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if names[parent] {
				return ErrInvalid
			}
			if directories[parent] {
				// Every ancestor of a counted directory is already counted.
				break
			}
			directories[parent] = true
			nodes++
			if nodes > MaxNodes {
				return ErrInvalid
			}
		}
	}
	return nil
}

// entry is the identity of one file without its bytes: what the tree digest
// and the tree checks need. Streaming readers keep entries, never contents.
type entry struct {
	path string
	mode uint32
	size int
	sum  [32]byte
}

// Summary is the identity of a validated bundle without its contents.
type Summary struct {
	SourceSHA    string
	Files        int
	ContentBytes int
	TreeDigest   string
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

// decodeData accepts only canonical padded standard base64: the length is
// checked before allocating, line breaks (which the decoder would skip) are
// refused, and Strict refuses non-zero padding bits, so exactly one encoding
// maps to each byte string.
func decodeData(v string) ([]byte, error) {
	if len(v) > base64.StdEncoding.EncodedLen(MaxFileBytes) || len(v)%4 != 0 || strings.ContainsAny(v, "\r\n") {
		return nil, ErrInvalid
	}
	data, e := base64.StdEncoding.Strict().DecodeString(v)
	if e != nil || len(data) > MaxFileBytes || base64.StdEncoding.EncodedLen(len(data)) != len(v) {
		return nil, ErrInvalid
	}
	return data, nil
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
			if e != nil {
				return f, ErrInvalid
			}
			if f.Data, e = decodeData(v); e != nil {
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

// scan parses one bundle document from input, one file at a time. It counts
// entries and decoded bytes while parsing, checks base64 size before
// allocating, and requires exact, unique keys at every level and nothing after
// the document. Each file is passed to visit (when not nil) and then dropped:
// scan itself retains only paths and, with hash, content digests. The whole
// tree is checked after the last file, so visit may see files of a bundle
// that is refused later; callers must not publish them before scan returns.
func scan(input io.Reader, hash bool, visit func(File) error) (Summary, []entry, error) {
	var s Summary
	d := json.NewDecoder(input)
	d.UseNumber()
	if delim(d, '{') != nil {
		return s, nil, ErrInvalid
	}
	seen := map[string]bool{}
	var entries []entry
	var names map[string]bool
	for d.More() {
		key, e := readString(d)
		if e != nil || seen[key] {
			return s, nil, ErrInvalid
		}
		seen[key] = true
		switch key {
		case "version":
			v, e := readString(d)
			if e != nil || v != Version {
				return s, nil, ErrInvalid
			}
		case "sourceSHA":
			s.SourceSHA, e = readString(d)
			if e != nil || !ValidSourceSHA(s.SourceSHA) {
				return s, nil, ErrInvalid
			}
		case "files":
			if delim(d, '[') != nil {
				return s, nil, ErrInvalid
			}
			entries, names = []entry{}, map[string]bool{}
			for d.More() {
				if len(entries) >= MaxFiles {
					return s, nil, ErrInvalid
				}
				f, e := decodeFile(d)
				if e != nil || names[f.Path] {
					return s, nil, ErrInvalid
				}
				s.ContentBytes += len(f.Data)
				if s.ContentBytes > MaxContentBytes {
					return s, nil, ErrInvalid
				}
				names[f.Path] = true
				item := entry{path: f.Path, mode: f.Mode, size: len(f.Data)}
				if hash {
					item.sum = sha256.Sum256(f.Data)
				}
				entries = append(entries, item)
				if visit != nil {
					if visit(f) != nil {
						return s, nil, ErrInvalid
					}
				}
			}
			if delim(d, ']') != nil {
				return s, nil, ErrInvalid
			}
		default:
			return s, nil, ErrInvalid
		}
	}
	if len(seen) != 3 || delim(d, '}') != nil {
		return s, nil, ErrInvalid
	}
	if _, e := d.Token(); e != io.EOF {
		return s, nil, ErrInvalid
	}
	if checkTree(names) != nil || canonicalSize(s.SourceSHA, entries) > MaxWireBytes {
		return s, nil, ErrInvalid
	}
	s.Files = len(entries)
	if hash {
		s.TreeDigest = treeDigest(s.SourceSHA, entries)
	}
	return s, entries, nil
}

// Decode counts entries and decoded bytes while parsing. Base64 is size-checked
// before allocating its decoded body. Keys are exact and unique at every level.
func Decode(raw []byte) (Bundle, error) {
	var b Bundle
	if len(raw) == 0 || len(raw) > MaxWireBytes || !utf8.Valid(raw) {
		return b, ErrInvalid
	}
	files := []File{}
	s, _, e := scan(bytes.NewReader(raw), false, func(f File) error { files = append(files, f); return nil })
	if e != nil {
		return b, ErrInvalid
	}
	b = Bundle{Version: Version, SourceSHA: s.SourceSHA, Files: files}
	return b, b.Validate()
}

// Inspect validates raw exactly as Decode does and returns its identity, the
// tree digest included, without keeping any file content: it holds raw, the
// paths and one decoded file at a time instead of a second full copy.
func Inspect(raw []byte) (Summary, error) {
	if len(raw) == 0 || len(raw) > MaxWireBytes || !utf8.Valid(raw) {
		return Summary{}, ErrInvalid
	}
	s, _, e := scan(bytes.NewReader(raw), true, nil)
	if e != nil {
		return Summary{}, ErrInvalid
	}
	return s, nil
}

// Encode returns exactly the bytes json.Marshal produces for b, written into
// one allocation of the final size; json.Marshal would hold its growing
// buffer and a copy, two to three times the wire size at the limits.
func Encode(b Bundle) ([]byte, error) {
	if b.Validate() != nil {
		return nil, ErrInvalid
	}
	size, e := encodedSize(b)
	if e != nil {
		return nil, ErrInvalid
	}
	raw := bytes.NewBuffer(make([]byte, 0, size))
	if EncodeTo(raw, b) != nil || raw.Len() != size {
		return nil, ErrInvalid
	}
	return raw.Bytes(), nil
}

// EncodeTo writes Encode(b) to w without building it in memory first. It
// validates b, including the wire size, before writing anything.
func EncodeTo(w io.Writer, b Bundle) error {
	if b.Validate() != nil {
		return ErrInvalid
	}
	if _, e := encodedSize(b); e != nil {
		return ErrInvalid
	}
	enc, e := newEncoder(w, b.SourceSHA)
	if e != nil {
		return ErrInvalid
	}
	for _, f := range b.Files {
		if enc.file(f) != nil {
			return ErrInvalid
		}
	}
	return enc.end()
}

// canonicalSize is len(Encode) of a tree known by its entries. Input may use
// shorter escapes than the canonical encoding (a literal '<' instead of
// \u003c), so a document within MaxWireBytes can re-encode over it; readers
// refuse such a tree as Encode and the export would.
func canonicalSize(sourceSHA string, entries []entry) int {
	size := len(headerBytes(sourceSHA)) + len("]}")
	for i, f := range entries {
		if i > 0 {
			size++
		}
		size += fileSize(File{Path: f.path, Mode: f.mode}) + base64.StdEncoding.EncodedLen(f.size)
		if size > MaxWireBytes {
			return size
		}
	}
	return size
}

// EncodedSize returns len(Encode(b)) without encoding: callers can check the
// wire bound of a tree they encode only later.
func EncodedSize(b Bundle) (int, error) {
	if b.Validate() != nil {
		return 0, ErrInvalid
	}
	return encodedSize(b)
}

// jsonString is the encoding/json form of v, HTML escaping included, so the
// streaming encoder stays byte-identical to json.Marshal.
func jsonString(v string) []byte {
	raw, _ := json.Marshal(v)
	return raw
}

const (
	fileOpen  = `{"path":`
	fileMode  = `,"mode":`
	fileData  = `,"data":"`
	fileClose = `"}`
)

// headerBytes needs no escaping: Version is a fixed ASCII constant and every
// caller has checked that sourceSHA is 40 lowercase hex digits.
func headerBytes(sourceSHA string) []byte {
	return []byte(`{"version":"` + Version + `","sourceSHA":"` + sourceSHA + `","files":[`)
}
func fileSize(f File) int {
	return len(fileOpen) + len(jsonString(f.Path)) + len(fileMode) + len(strconv.FormatUint(uint64(f.Mode), 10)) + len(fileData) + base64.StdEncoding.EncodedLen(len(f.Data)) + len(fileClose)
}
func encodedSize(b Bundle) (int, error) {
	size := len(headerBytes(b.SourceSHA)) + len("]}")
	for i, f := range b.Files {
		if i > 0 {
			size++
		}
		size += fileSize(f)
		if size > MaxWireBytes {
			return 0, ErrInvalid
		}
	}
	return size, nil
}

// encoder streams one bundle document, counting its wire bytes. A failure
// can leave a partial document on w: callers must discard the output.
type encoder struct {
	w       io.Writer
	written int
	files   int
}

func newEncoder(w io.Writer, sourceSHA string) (*encoder, error) {
	if !ValidSourceSHA(sourceSHA) {
		return nil, ErrInvalid
	}
	e := &encoder{w: w}
	return e, e.write(headerBytes(sourceSHA))
}
func (e *encoder) write(p []byte) error {
	if e.written+len(p) > MaxWireBytes {
		return ErrInvalid
	}
	n, err := e.w.Write(p)
	e.written += n
	if err != nil || n != len(p) {
		return ErrInvalid
	}
	return nil
}
func (e *encoder) file(f File) error {
	if e.files >= MaxFiles || len(f.Path) > MaxPathBytes || len(f.Data) > MaxFileBytes {
		return ErrInvalid
	}
	// Pieces are written straight to w; base64 streams through a fixed
	// internal buffer, so no allocation depends on the file's size.
	prefix := []byte(fileOpen)
	if e.files > 0 {
		prefix = []byte("," + fileOpen)
	}
	prefix = append(prefix, jsonString(f.Path)...)
	prefix = append(prefix, fileMode...)
	prefix = strconv.AppendUint(prefix, uint64(f.Mode), 10)
	prefix = append(prefix, fileData...)
	if e.write(prefix) != nil {
		return ErrInvalid
	}
	data := base64.NewEncoder(base64.StdEncoding, encoderWriter{e})
	if _, err := data.Write(f.Data); err != nil || data.Close() != nil {
		return ErrInvalid
	}
	e.files++
	return e.write([]byte(fileClose))
}

// encoderWriter routes base64 output through the encoder's wire count.
type encoderWriter struct{ e *encoder }

func (w encoderWriter) Write(p []byte) (int, error) {
	if w.e.write(p) != nil {
		return 0, ErrInvalid
	}
	return len(p), nil
}
func (e *encoder) end() error { return e.write([]byte("]}")) }

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

// wireReader bounds a streamed bundle as Decode bounds a byte slice: at most
// MaxWireBytes, valid UTF-8 across read boundaries, at least one byte. It
// hashes what it passes on, so the digest covers exactly the parsed input.
type wireReader struct {
	r       io.Reader
	h       hash.Hash
	n       int
	pending []byte
	err     error
}

func newWireReader(r io.Reader) *wireReader { return &wireReader{r: r, h: sha256.New()} }
func (w *wireReader) Read(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	n, err := w.r.Read(p)
	if err != nil && err != io.EOF {
		// Kept even when the decoder never asks again: a read error is a
		// refusal however complete the bytes before it looked.
		w.err = ErrInvalid
	}
	w.n += n
	if w.n > MaxWireBytes {
		w.err = ErrInvalid
		return 0, w.err
	}
	w.h.Write(p[:n])
	// Validate complete runes; carry at most three bytes of a rune that a
	// read boundary split.
	chunk := append(w.pending, p[:n]...)
	cut := len(chunk)
	for back := 1; back <= 3 && back <= len(chunk); back++ {
		if utf8.RuneStart(chunk[len(chunk)-back]) {
			if !utf8.FullRune(chunk[len(chunk)-back:]) {
				cut = len(chunk) - back
			}
			break
		}
	}
	if !utf8.Valid(chunk[:cut]) {
		w.err = ErrInvalid
		return 0, w.err
	}
	w.pending = append([]byte(nil), chunk[cut:]...)
	if err == io.EOF && (len(w.pending) != 0 || w.n == 0) {
		w.err = ErrInvalid
		return 0, w.err
	}
	return n, err
}

// digest returns the SHA-256 of the input read so far; valid only after the
// reader reached EOF without error.
func (w *wireReader) digest() string { return hex.EncodeToString(w.h.Sum(nil)) }

// TreeDigest binds a validated file tree independently of JSON formatting or
// input order. The wire encoding is versioned and domain-separated: prefix,
// 20-byte source commit, u32 file count, then UTF-8-byte-sorted entries with u32
// path length, path bytes, u32 Git mode, u64 data length and 32-byte content hash.
// Integers are unsigned big-endian. No source file or caller slice is mutated.
func (b Bundle) TreeDigest() (string, error) {
	if b.Validate() != nil {
		return "", ErrInvalid
	}
	entries := make([]entry, len(b.Files))
	for i, f := range b.Files {
		entries[i] = entry{path: f.Path, mode: f.Mode, size: len(f.Data), sum: sha256.Sum256(f.Data)}
	}
	return treeDigest(b.SourceSHA, entries), nil
}

// treeDigest sorts a copy of entries and hashes the documented wire format.
func treeDigest(sourceSHA string, entries []entry) string {
	entries = append([]entry(nil), entries...)
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	h := sha256.New()
	h.Write([]byte("ai-sdlc.workspace-tree/v1\x00"))
	source, _ := hex.DecodeString(sourceSHA)
	h.Write(source)
	var u32 [4]byte
	var u64 [8]byte
	binary.BigEndian.PutUint32(u32[:], uint32(len(entries)))
	h.Write(u32[:])
	for _, f := range entries {
		binary.BigEndian.PutUint32(u32[:], uint32(len(f.path)))
		h.Write(u32[:])
		h.Write([]byte(f.path))
		binary.BigEndian.PutUint32(u32[:], f.mode)
		h.Write(u32[:])
		binary.BigEndian.PutUint64(u64[:], uint64(f.size))
		h.Write(u64[:])
		h.Write(f.sum[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}
