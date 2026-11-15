package sourcefetch

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

const testSHA = "0622cdaeb59a11a27bd5f3d7e18183c401a5e412"
const testRoot = "fixture-product-0622cda/"

func archiveFixture(t *testing.T, headers []*tar.Header) []byte {
	t.Helper()
	var output bytes.Buffer
	gz := gzip.NewWriter(&output)
	writer := tar.NewWriter(gz)
	if err := writer.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": testSHA}}); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: testRoot, Mode: 0755}); err != nil {
		t.Fatal(err)
	}
	for _, h := range headers {
		if err := writer.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg && h.Size > 0 {
			if _, err := writer.Write(bytes.Repeat([]byte("x"), int(h.Size))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if writer.Close() != nil || gz.Close() != nil {
		t.Fatal("fixture archive failed")
	}
	return output.Bytes()
}
func TestArchiveRegularFilesAndUnsafeNodes(t *testing.T) {
	safe := &tar.Header{Typeflag: tar.TypeReg, Name: testRoot + "README.md", Mode: 0644, Size: 3}
	raw := archiveFixture(t, []*tar.Header{safe})
	b, err := DecodeArchive(raw, testSHA)
	if err != nil || len(b.Files) != 1 || string(b.Files[0].Data) != "xxx" {
		t.Fatalf("regular archive: %v files=%d", err, len(b.Files))
	}
	cases := []*tar.Header{
		{Typeflag: tar.TypeSymlink, Name: testRoot + "link", Linkname: "/etc/passwd"},
		{Typeflag: tar.TypeLink, Name: testRoot + "hard", Linkname: testRoot + "README.md"},
		{Typeflag: tar.TypeFifo, Name: testRoot + "fifo"},
		{Typeflag: tar.TypeReg, Name: testRoot + "../escape", Mode: 0644},
		{Typeflag: tar.TypeReg, Name: testRoot + ".git/config", Mode: 0644},
		{Typeflag: tar.TypeReg, Name: "/absolute", Mode: 0644},
		{Typeflag: tar.TypeReg, Name: testRoot + "setuid", Mode: 04755},
		{Typeflag: tar.TypeReg, Name: testRoot + "large", Mode: 0644, Size: 4*1024*1024 + 1},
	}
	for _, h := range cases {
		t.Run(h.Name, func(t *testing.T) {
			if _, err := DecodeArchive(archiveFixture(t, []*tar.Header{h}), testSHA); err == nil {
				t.Fatal("unsafe archive accepted")
			}
		})
	}
	if _, err := DecodeArchive(archiveFixture(t, []*tar.Header{safe, safe}), testSHA); err == nil {
		t.Fatal("duplicate path accepted")
	}
	if _, err := DecodeArchive(append(raw, raw...), testSHA); err == nil {
		t.Fatal("concatenated gzip accepted")
	}
	if _, err := DecodeArchive(raw, strings.Repeat("a", 40)); err == nil {
		t.Fatal("wrong commit accepted")
	}
	if _, err := DecodeArchive(raw[:len(raw)-4], testSHA); err == nil {
		t.Fatal("truncated gzip accepted")
	}
}
func TestActualPinnedHomedirArchive(t *testing.T) {
	file := os.Getenv("AI_SDLC_TEST_SOURCE_ARCHIVE")
	if file == "" {
		t.Skip("actual downloaded public archive required")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	b, err := DecodeArchive(raw, testSHA)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Files) != 1158 {
		t.Fatalf("actual Homedir files=%d", len(b.Files))
	}
	digest, err := b.TreeDigest()
	if err != nil || digest != "a392d4d3be50cc89cb1cc324fb736952fe9e86b47e5d131912163d363c147277" {
		t.Fatalf("archive differs from independently exported pinned Homedir tree: %s %v", digest, err)
	}
}

func TestArchiveRejectsEmptyDirectoryFileConflicts(t *testing.T) {
	file := &tar.Header{Typeflag: tar.TypeReg, Name: testRoot + "parent", Mode: 0644}
	directory := &tar.Header{Typeflag: tar.TypeDir, Name: testRoot + "parent/empty/", Mode: 0755}
	for _, headers := range [][]*tar.Header{{file, directory}, {directory, file}} {
		if _, err := DecodeArchive(archiveFixture(t, headers), testSHA); err == nil {
			t.Fatal("file/directory conflict was silently discarded")
		}
	}
}

func TestArchiveBoundsAndTrailingContent(t *testing.T) {
	raw := archiveFixture(t, nil)
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	expanded, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	gz.Close()
	for name, tail := range map[string][]byte{"expanded-overflow": make([]byte, MaxArchiveBytes), "trailing-junk": []byte("hidden"), "second-tar": expanded} {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			writer := gzip.NewWriter(&output)
			writer.Write(expanded)
			writer.Write(tail)
			writer.Close()
			if _, err := DecodeArchive(output.Bytes(), testSHA); err == nil {
				t.Fatal("archive tail exceeded contract")
			}
		})
	}
	for name, count := range map[string]int{"files": 4097, "directories": 8193} {
		t.Run(name, func(t *testing.T) {
			headers := make([]*tar.Header, 0, count)
			for i := 0; i < count; i++ {
				kind := byte(tar.TypeReg)
				mode := int64(0644)
				if name == "directories" {
					kind = tar.TypeDir
					mode = 0755
				}
				headers = append(headers, &tar.Header{Typeflag: kind, Name: fmt.Sprintf("%snode-%d", testRoot, i), Mode: mode})
			}
			if _, err := DecodeArchive(archiveFixture(t, headers), testSHA); err == nil {
				t.Fatal("node/file bound ignored")
			}
		})
	}
}
