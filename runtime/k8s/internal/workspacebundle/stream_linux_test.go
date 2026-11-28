package workspacebundle

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// largeFixture is a repository of the measured class (about 40 MB in 2000
// files, nested modules, a few multi-megabyte files): over the earlier 16 MiB
// content and 32 MiB wire limits, within the current ones.
func largeFixture(t testing.TB) Bundle {
	t.Helper()
	random := rand.New(rand.NewSource(1))
	b := Bundle{Version: Version, SourceSHA: strings.Repeat("c", 40), Files: []File{}}
	total := 0
	for i := 0; i < 2000; i++ {
		size := 14 * 1024
		if i%500 == 0 {
			size = 3*1024*1024 + i
		}
		data := make([]byte, size)
		random.Read(data)
		mode := uint32(0644)
		if i%97 == 0 {
			mode = 0755
		}
		name := fmt.Sprintf("module-%02d/src/main/java/io/quarkus/sample/p%03d/File%04d.java", i%20, i%150, i)
		b.Files = append(b.Files, File{Path: name, Mode: mode, Data: data})
		total += size
	}
	if total < 40*1000*1000 || total <= 16*1024*1024 {
		t.Fatalf("fixture has %d bytes", total)
	}
	return b
}

func TestLargeRepositoryPassesDecodeValidationAndTransfer(t *testing.T) {
	b := largeFixture(t)
	if b.Validate() != nil {
		t.Fatal("large bundle refused by Validate")
	}
	raw, err := Encode(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) <= 32*1024*1024 || len(raw) > MaxWireBytes {
		t.Fatalf("wire size %d", len(raw))
	}
	decoded, err := Decode(raw)
	if err != nil || !reflect.DeepEqual(decoded, b) {
		t.Fatal("decode", err)
	}
	verified, err := ReadVerified(bytes.NewReader(raw), Digest(raw), b.SourceSHA)
	if err != nil || len(verified.Files) != len(b.Files) {
		t.Fatal("read verified", err)
	}
	tree, err := b.TreeDigest()
	if err != nil {
		t.Fatal(err)
	}
	summary, err := Inspect(raw)
	if err != nil || summary != (Summary{SourceSHA: b.SourceSHA, Files: 2000, ContentBytes: contentBytes(b), TreeDigest: tree}) {
		t.Fatalf("inspect %+v %v", summary, err)
	}
	parent := t.TempDir()
	imported, err := ImportFrom(parent, iotest.HalfReader(bytes.NewReader(raw)), Digest(raw), b.SourceSHA)
	if err != nil || imported != summary {
		t.Fatalf("import %+v %v", imported, err)
	}
	exported, err := ExportSummary(parent, b.SourceSHA)
	if err != nil || exported != summary {
		t.Fatalf("export summary %+v %v", exported, err)
	}
	var streamed bytes.Buffer
	// Export lists the tree in path order.
	sorted := Bundle{Version: Version, SourceSHA: b.SourceSHA, Files: append([]File(nil), b.Files...)}
	sort.Slice(sorted.Files, func(i, j int) bool { return sorted.Files[i].Path < sorted.Files[j].Path })
	want, _ := Encode(sorted)
	if err = ExportTo(parent, b.SourceSHA, &streamed); err != nil || !bytes.Equal(streamed.Bytes(), want) {
		t.Fatal("streamed export differs", err)
	}
}

func contentBytes(b Bundle) int {
	total := 0
	for _, f := range b.Files {
		total += len(f.Data)
	}
	return total
}

// peakHeap runs fn with a low GC target and samples the heap in use, so the
// result approximates the live peak instead of uncollected garbage.
func peakHeap(t *testing.T, fn func() error) uint64 {
	t.Helper()
	defer debug.SetGCPercent(debug.SetGCPercent(5))
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	stop, done := make(chan struct{}), make(chan uint64)
	go func() {
		peak := uint64(0)
		for {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			peak = max(peak, m.HeapInuse)
			select {
			case <-stop:
				done <- peak
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	err := fn()
	// A last sample after fn, while its result is still referenced by the
	// caller, so a fast fn cannot finish between two samples.
	var last runtime.MemStats
	runtime.ReadMemStats(&last)
	close(stop)
	peak := max(<-done, last.HeapInuse)
	if err != nil {
		t.Fatal(err)
	}
	if peak < base.HeapInuse {
		return 0
	}
	return peak - base.HeapInuse
}

// The streaming readers hold the paths and one file, not a second copy of
// the tree: inspecting, importing and exporting the 40 MB bundle keeps a
// bound set by the largest file (its base64 token, decoder buffer and bytes)
// in use, while Decode keeps all of the content.
func TestStreamingReadersDoNotCopyTheTree(t *testing.T) {
	b := largeFixture(t)
	raw, err := Encode(b)
	if err != nil {
		t.Fatal(err)
	}
	content := uint64(contentBytes(b))
	b = Bundle{}
	var kept Bundle
	peak := peakHeap(t, func() (e error) { kept, e = Decode(raw); return e })
	if peak < content {
		t.Fatalf("measure too weak: Decode peak %d below content %d", peak, content)
	}
	t.Logf("decode: %d bytes in use for %d bytes of content (%d on the wire)", peak, content, len(raw))
	runtime.KeepAlive(kept)
	const bound = 8 * MaxFileBytes
	kept = Bundle{}
	parent := t.TempDir()
	for name, fn := range map[string]func() error{
		"inspect": func() error { _, e := Inspect(raw); return e },
		"import": func() error {
			_, e := ImportFrom(parent, bytes.NewReader(raw), Digest(raw), strings.Repeat("c", 40))
			return e
		},
	} {
		peak := peakHeap(t, fn)
		if peak > bound {
			t.Fatalf("%s peak %d bytes for %d bytes of content", name, peak, content)
		}
		t.Logf("%s: %d bytes in use", name, peak)
	}
	peak = peakHeap(t, func() error { return ExportTo(parent, strings.Repeat("c", 40), io.Discard) })
	if peak > bound {
		t.Fatalf("export peak %d bytes for %d bytes of content", peak, content)
	}
	t.Logf("export: %d bytes in use", peak)
	_ = kept
}

func TestEncodeIsByteIdenticalToJSONMarshal(t *testing.T) {
	b := fixture()
	b.Files = append(b.Files, File{"a<b>&\"c d e'f", 0755, []byte{0, 1, 2, 0xff}}, File{"empty", 0644, []byte{}})
	want, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Encode(b)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("encode differs:\n%s\n%s", got, want)
	}
	if size, err := EncodedSize(b); err != nil || size != len(want) {
		t.Fatal("encoded size", size, err)
	}
	var streamed bytes.Buffer
	if err = EncodeTo(&streamed, b); err != nil || !bytes.Equal(streamed.Bytes(), want) {
		t.Fatal("streamed encode differs", err)
	}
	b.Files = []File{}
	want, _ = json.Marshal(b)
	if got, err = Encode(b); err != nil || !bytes.Equal(got, want) {
		t.Fatal("empty tree differs", err)
	}
	b.Files = append(b.Files, File{"../x", 0644, []byte{}})
	var refused bytes.Buffer
	if EncodeTo(&refused, b) == nil || refused.Len() != 0 {
		t.Fatal("invalid bundle written")
	}
}

func TestInspectMatchesDecode(t *testing.T) {
	raw, _ := Encode(fixture())
	summary, err := Inspect(raw)
	tree, _ := fixture().TreeDigest()
	if err != nil || summary.TreeDigest != tree || summary.Files != 4 || summary.SourceSHA != fixture().SourceSHA {
		t.Fatalf("%+v %v", summary, err)
	}
	for _, bad := range []string{
		strings.Replace(string(raw), `"data":"aGVsbG8K"`, `"data":"aGVsbG8K\n"`, 1),
		strings.Replace(string(raw), `"data":"aGVsbG8K"`, `"data":"aGVsbG9="`, 1),
		strings.Replace(string(raw), `"path":"zero"`, `"path":"README.md"`, 1),
		strings.Replace(string(raw), `"path":"zero"`, `"path":"README.md/x"`, 1),
		string(raw) + " x", "",
	} {
		_, decodeErr := Decode([]byte(bad))
		if _, err := Inspect([]byte(bad)); err == nil || decodeErr == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestImportFromPublishesOnlyAVerifiedTree(t *testing.T) {
	b := fixture()
	raw, _ := Encode(b)
	for name, c := range map[string]struct {
		raw    []byte
		digest string
		source string
	}{
		"digest":     {raw, strings.Repeat("0", 64), b.SourceSHA},
		"source":     {raw, Digest(raw), strings.Repeat("b", 40)},
		"truncated":  {raw[:len(raw)-1], Digest(raw[:len(raw)-1]), b.SourceSHA},
		"trailing":   {append(bytes.Clone(raw), "{}"...), Digest(append(bytes.Clone(raw), "{}"...)), b.SourceSHA},
		"invalid":    {[]byte(strings.Replace(string(raw), `"README.md"`, `"`+string([]byte{0xff})+`"`, 1)), "", b.SourceSHA},
		"conflict":   {[]byte(strings.Replace(string(raw), `"path":"zero"`, `"path":"README.md/zero"`, 1)), "", b.SourceSHA},
		"duplicate":  {[]byte(strings.Replace(string(raw), `"path":"zero"`, `"path":"README.md"`, 1)), "", b.SourceSHA},
		"empty":      {[]byte{}, Digest(nil), b.SourceSHA},
		"split-rune": {[]byte(strings.Replace(string(raw), "日本語", string([]byte{0xe6, 0x97}), 1)), "", b.SourceSHA},
	} {
		t.Run(name, func(t *testing.T) {
			digest := c.digest
			if digest == "" {
				digest = Digest(c.raw)
			}
			parent := t.TempDir()
			if _, err := ImportFrom(parent, iotest.OneByteReader(bytes.NewReader(c.raw)), digest, c.source); err == nil {
				t.Fatal("accepted")
			}
			if _, err := os.Lstat(filepath.Join(parent, "repository")); !os.IsNotExist(err) {
				t.Fatal("repository published for a refused import")
			}
			if _, err := os.Lstat(filepath.Join(parent, stagingName)); !os.IsNotExist(err) {
				t.Fatal("staging left after a refused import")
			}
		})
	}
	// A staging directory from an interrupted import (with a link inside,
	// which is removed, never followed) does not block a valid import.
	retry := t.TempDir()
	outside := filepath.Join(t.TempDir(), "keep")
	if err := os.WriteFile(outside, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(retry, stagingName, "a", "b"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(retry, stagingName, "a", "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportFrom(retry, bytes.NewReader(raw), Digest(raw), b.SourceSHA); err != nil {
		t.Fatal("stale staging blocked a valid import", err)
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "keep" {
		t.Fatal("removal followed a link")
	}
	parent := t.TempDir()
	// A rune split across every read boundary is still valid UTF-8.
	summary, err := ImportFrom(parent, iotest.OneByteReader(bytes.NewReader(raw)), Digest(raw), b.SourceSHA)
	if err != nil || summary.Files != len(b.Files) {
		t.Fatal(err)
	}
	if exported, err := Export(parent, b.SourceSHA); err != nil || !reflect.DeepEqual(exported, b) {
		t.Fatal("imported tree differs", err)
	}
	if _, err := os.Lstat(filepath.Join(parent, stagingName)); !os.IsNotExist(err) {
		t.Fatal("staging directory left after success")
	}
	if _, err := ImportFrom(parent, bytes.NewReader(raw), Digest(raw), b.SourceSHA); err == nil {
		t.Fatal("imported over an existing repository")
	}
	link := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(link, "repository")); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportFrom(link, bytes.NewReader(raw), Digest(raw), b.SourceSHA); err == nil {
		t.Fatal("imported over a symlink")
	}
	over := t.TempDir()
	if _, err := ImportFrom(over, ioRepeatReader{}, Digest(raw), b.SourceSHA); err == nil {
		t.Fatal("unbounded input accepted")
	}
}

func TestExportRefusesAFileChangedAfterListing(t *testing.T) {
	parent := t.TempDir()
	b := fixture()
	if err := Import(parent, b); err != nil {
		t.Fatal(err)
	}
	calls := 0
	err := walkTree(parent, b.SourceSHA, func(f File) error {
		calls++
		if calls == 1 {
			// The next file in path order is rewritten after the listing.
			return os.WriteFile(filepath.Join(parent, "repository", "bin", "check"), []byte("changed"), 0755)
		}
		return nil
	})
	if err == nil {
		t.Fatal("changed file exported")
	}
	var out bytes.Buffer
	if err := os.WriteFile(filepath.Join(parent, "repository", "big"), make([]byte, MaxFileBytes+1), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ExportTo(parent, b.SourceSHA, &out); RefusalReason(err) != ExportFileSize || out.Len() != 0 {
		t.Fatal("refusal after writing output", err)
	}
}

// Paths that JSON escapes sixfold can push the encoding over MaxWireBytes
// while the content stays within its bound; the listing refuses that with
// its reason before writing.
func TestExportRefusesAnOversizedEncodingBeforeWriting(t *testing.T) {
	parent := t.TempDir()
	b := fixture()
	b.Files = []File{}
	if err := Import(parent, b); err != nil {
		t.Fatal(err)
	}
	// 32 files of 4 MiB are exactly the content bound (about 170.7 MiB of
	// base64); 16,000 empty files whose names hold 245 '<' (each escaped to
	// six bytes) add about 23 MiB of JSON: over 192 MiB on the wire.
	root := filepath.Join(parent, "repository")
	big := make([]byte, MaxFileBytes)
	for i := 0; i < MaxContentBytes/MaxFileBytes; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprint("big", i)), big, 0644); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 16000; i++ {
		dir := filepath.Join(root, fmt.Sprintf("d%02d", i%50))
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		name := fmt.Sprintf("%05d", i) + strings.Repeat("<", 245)
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0644); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := ExportTo(parent, b.SourceSHA, &out); RefusalReason(err) != ExportTotalSize || out.Len() != 0 {
		t.Fatal("oversized encoding not refused before writing", err, out.Len())
	}
}

// A read error is a refusal even when the bytes before it complete a valid
// document and the decoder never reads again.
func TestImportFromRefusesAReadError(t *testing.T) {
	b := fixture()
	raw, _ := Encode(b)
	parent := t.TempDir()
	input := &errorAfter{data: raw}
	if _, err := ImportFrom(parent, input, Digest(raw), b.SourceSHA); err == nil {
		t.Fatal("import published despite a read error")
	}
	if _, err := os.Lstat(filepath.Join(parent, "repository")); !os.IsNotExist(err) {
		t.Fatal("repository published after a read error")
	}
}

// errorAfter returns all its bytes together with a non-EOF error, then EOF.
type errorAfter struct{ data []byte }

func (e *errorAfter) Read(p []byte) (int, error) {
	if len(e.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, e.data)
	e.data = e.data[n:]
	if len(e.data) > 0 {
		return n, nil
	}
	return n, fmt.Errorf("device error")
}

// Literal '<' is one byte on input but six in the canonical encoding: a
// document under MaxWireBytes whose canonical encoding exceeds it is refused,
// as Encode and the export would refuse it.
func TestReadersRefuseATreeWhoseCanonicalEncodingIsOversized(t *testing.T) {
	var doc bytes.Buffer
	doc.WriteString(`{"version":"` + Version + `","sourceSHA":"` + strings.Repeat("a", 40) + `","files":[`)
	data := base64.StdEncoding.EncodeToString(make([]byte, MaxFileBytes))
	for i := 0; i < MaxContentBytes/MaxFileBytes; i++ {
		fmt.Fprintf(&doc, `{"path":"big%d","mode":420,"data":"%s"},`, i, data)
	}
	for i := 0; i < 16000; i++ {
		if i > 0 {
			doc.WriteByte(',')
		}
		fmt.Fprintf(&doc, `{"path":"d%02d/%05d%s","mode":420,"data":""}`, i%50, i, strings.Repeat("<", 245))
	}
	doc.WriteString("]}")
	raw := doc.Bytes()
	if len(raw) > MaxWireBytes {
		t.Fatalf("fixture %d bytes", len(raw))
	}
	if _, err := Inspect(raw); err == nil {
		t.Fatal("Inspect accepted an oversized canonical encoding")
	}
	parent := t.TempDir()
	if _, err := ImportFrom(parent, bytes.NewReader(raw), Digest(raw), strings.Repeat("a", 40)); err == nil {
		t.Fatal("ImportFrom accepted an oversized canonical encoding")
	}
	if _, err := os.Lstat(filepath.Join(parent, "repository")); !os.IsNotExist(err) {
		t.Fatal("repository published")
	}
}
