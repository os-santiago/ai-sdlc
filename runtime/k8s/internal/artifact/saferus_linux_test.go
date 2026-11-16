package artifact

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

var staticFixture struct {
	sync.Once
	raw []byte
	err error
}

func saferusFixture(t *testing.T) string {
	t.Helper()
	staticFixture.Do(func() {
		root := t.TempDir()
		source := filepath.Join(root, "main.go")
		if staticFixture.err = os.WriteFile(source, []byte("package main\nfunc main(){println(\"trusted synthetic artifact fixture\")}\n"), 0600); staticFixture.err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-ldflags=-s -w", "-o", filepath.Join(root, "out"), source)
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64", "GOWORK=off", "GOFLAGS=")
		if _, staticFixture.err = cmd.CombinedOutput(); staticFixture.err != nil {
			return
		}
		staticFixture.raw, staticFixture.err = os.ReadFile(filepath.Join(root, "out"))
	})
	if staticFixture.err != nil {
		t.Fatal("compile trusted synthetic fixture", staticFixture.err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "saferus"), staticFixture.raw, 0755); err != nil {
		t.Fatal(err)
	}
	return root
}
func TestSaferusCanonicalStaticImageHasNoInheritedAuthority(t *testing.T) {
	root := saferusFixture(t)
	image, report, err := AssembleSaferus(context.Background(), root, identity)
	if err != nil {
		t.Fatal(err)
	}
	if report.Recipe != SaferusRecipe || report.Base != "scratch" || len(report.Files) != 1 || report.ImageDigest == "" {
		t.Fatal("missing artifact evidence")
	}
	cfg, err := image.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Config.User != "1000:1000" || len(cfg.Config.Env) != 0 || len(cfg.Config.Cmd) != 0 || len(cfg.Config.Volumes) != 0 || len(cfg.Config.Entrypoint) != 1 || cfg.Config.Entrypoint[0] != "/saferus" || cfg.Config.Labels["org.opencontainers.image.source"] != SaferusSourceURL {
		t.Fatal("unexpected runtime config")
	}
	layers, err := image.Layers()
	if err != nil || len(layers) != 1 {
		t.Fatal("scratch must have one layer")
	}
	stream, err := layers[0].Uncompressed()
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	reader := tar.NewReader(stream)
	h, err := reader.Next()
	if err != nil || h.Name != "saferus" || h.Mode != 0755 || h.Uid != 1000 || h.Gid != 1000 || h.ModTime.Unix() != 0 {
		t.Fatal("bad layer entry")
	}
	raw, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(raw, staticFixture.raw) {
		t.Fatal("binary bytes changed")
	}
	if _, err = reader.Next(); err != io.EOF {
		t.Fatal("unexpected layer entry")
	}
	again, _, err := AssembleSaferus(context.Background(), root, identity)
	if err != nil {
		t.Fatal(err)
	}
	one, _ := image.Digest()
	two, _ := again.Digest()
	if one != two {
		t.Fatal("non-deterministic assembly")
	}
}
func TestSaferusRejectsLinksExtrasSpecialModesAndInvalidELF(t *testing.T) {
	for _, mode := range []string{"symlink", "hardlink", "directory", "fifo", "extra", "setuid", "not-elf", "wrong-platform", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			root := saferusFixture(t)
			p := filepath.Join(root, "saferus")
			switch mode {
			case "symlink":
				target := filepath.Join(t.TempDir(), "target")
				os.Rename(p, target)
				os.Symlink(target, p)
			case "hardlink":
				os.Link(p, filepath.Join(t.TempDir(), "outside-link"))
			case "directory":
				os.Remove(p)
				os.Mkdir(p, 0755)
			case "fifo":
				os.Remove(p)
				unix.Mkfifo(p, 0755)
			case "extra":
				os.WriteFile(filepath.Join(root, "secret"), []byte("must not package"), 0600)
			case "setuid":
				unix.Chmod(p, 04755)
			case "not-elf":
				os.WriteFile(p, []byte(strings.Repeat("x", 128)), 0755)
			case "wrong-platform":
				b := bytes.Clone(staticFixture.raw)
				b[18] = 0
				b[19] = 0
				os.WriteFile(p, b, 0755)
			case "oversize":
				os.Truncate(p, MaxSaferusBytes+1)
			}
			if _, _, err := AssembleSaferus(context.Background(), root, identity); err == nil {
				t.Fatal("unsafe package accepted")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := AssembleSaferus(ctx, saferusFixture(t), identity); err == nil {
		t.Fatal("cancel ignored")
	}
	root := saferusFixture(t)
	alias := filepath.Join(t.TempDir(), "alias")
	os.Symlink(root, alias)
	if _, _, err := AssembleSaferus(context.Background(), alias, identity); err == nil {
		t.Fatal("ancestor symlink accepted")
	}
}

func TestSaferusELFMetadataIsBoundedBeforeLibraryParsing(t *testing.T) {
	saferusFixture(t)
	for _, scenario := range []string{"section-count", "program-count", "extended-numbering", "section-offset-overflow", "program-offset-overflow", "section-entry-size", "compressed-names", "huge-names", "wrong-string-table"} {
		t.Run(scenario, func(t *testing.T) {
			raw := bytes.Clone(staticFixture.raw)
			le := binary.LittleEndian
			off := le.Uint64(raw[40:48]) + uint64(le.Uint16(raw[62:64]))*64
			switch scenario {
			case "section-count":
				le.PutUint16(raw[60:62], 65535)
			case "program-count":
				le.PutUint16(raw[56:58], 65535)
			case "extended-numbering":
				le.PutUint16(raw[60:62], 0)
			case "section-offset-overflow":
				le.PutUint64(raw[40:48], ^uint64(0))
			case "program-offset-overflow":
				le.PutUint64(raw[32:40], ^uint64(0))
			case "section-entry-size":
				le.PutUint16(raw[58:60], 1)
			case "compressed-names":
				le.PutUint64(raw[off+8:off+16], 0x800)
			case "huge-names":
				le.PutUint64(raw[off+32:off+40], 16*1024+1)
			case "wrong-string-table":
				le.PutUint32(raw[off+4:off+8], 8)
			}
			if staticLinuxAMD64(raw) {
				t.Fatal("hostile ELF metadata accepted")
			}
			if allocations := testing.AllocsPerRun(5, func() {
				if staticLinuxAMD64(raw) {
					panic("accepted")
				}
			}); allocations != 0 {
				t.Fatalf("preflight allocated: %v", allocations)
			}
		})
	}
	for n := 0; n < 64; n++ {
		if staticLinuxAMD64(staticFixture.raw[:n]) {
			t.Fatal("truncated header accepted")
		}
	}
}
