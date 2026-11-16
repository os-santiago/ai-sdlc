package artifact

import (
	"archive/tar"
	"bytes"
	"context"
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
