package artifact

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"golang.org/x/sys/unix"
)

var identity = Identity{strings.Repeat("a", 40), "sha256:" + strings.Repeat("b", 64), "pipeline-run-123"}

func TestAssembleLayerMatchesManifestAndContainsGzip(t *testing.T) {
	for _, format := range []struct{ manifest, config, layer types.MediaType }{
		{types.DockerManifestSchema2, types.DockerConfigJSON, types.DockerLayer},
		{types.OCIManifestSchema1, types.OCIConfigJSON, types.OCILayer},
	} {
		t.Run(string(format.manifest), func(t *testing.T) {
			root, base := fixture(t)
			base = mutate.ConfigMediaType(mutate.MediaType(base, format.manifest), format.config)
			img, _, err := Assemble(context.Background(), base, root, identity)
			if err != nil {
				t.Fatal(err)
			}
			manifest, err := img.Manifest()
			if err != nil {
				t.Fatal(err)
			}
			if manifest.MediaType != format.manifest || manifest.Config.MediaType != format.config || manifest.Layers[len(manifest.Layers)-1].MediaType != format.layer {
				t.Fatalf("incompatible image media types: %+v", manifest)
			}
			layers, err := img.Layers()
			if err != nil {
				t.Fatal(err)
			}
			compressed, err := layers[len(layers)-1].Compressed()
			if err != nil {
				t.Fatal(err)
			}
			defer compressed.Close()
			decoded, err := gzip.NewReader(compressed)
			if err != nil {
				t.Fatal(err)
			}
			defer decoded.Close()
			uncompressed, err := io.ReadAll(decoded)
			if err != nil {
				t.Fatal(err)
			}
			diffID, _, err := v1.SHA256(bytes.NewReader(uncompressed))
			if err != nil {
				t.Fatal(err)
			}
			expectedDiffID, err := layers[len(layers)-1].DiffID()
			if err != nil || diffID != expectedDiffID {
				t.Fatalf("uncompressed identity mismatch: %v", err)
			}
			again, err := layers[len(layers)-1].Compressed()
			if err != nil {
				t.Fatal(err)
			}
			defer again.Close()
			compressedID, _, err := v1.SHA256(again)
			if err != nil {
				t.Fatal(err)
			}
			expectedID, err := layers[len(layers)-1].Digest()
			if err != nil || compressedID != expectedID {
				t.Fatalf("compressed identity mismatch: %v", err)
			}
			header, err := tar.NewReader(bytes.NewReader(uncompressed)).Next()
			if err != nil || header.Name != "deployments/" {
				t.Fatalf("invalid compressed tar: %v", err)
			}
		})
	}
}

func fixture(t *testing.T) (string, v1.Image) {
	t.Helper()
	root := t.TempDir()
	for _, p := range []string{"app/app.jar", "lib/main/dependency.jar", "quarkus/generated.jar", "quarkus-run.jar"} {
		f := filepath.Join(root, p)
		if e := os.MkdirAll(filepath.Dir(f), 0755); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(f, []byte(p), 0755); e != nil {
			t.Fatal(e)
		}
	}
	cfg := &v1.ConfigFile{OS: "linux", Architecture: "amd64", Config: v1.Config{Env: []string{"UNTRUSTED=yes"}, Entrypoint: []string{"/bad"}, Volumes: map[string]struct{}{"/data": {}}, Labels: map[string]string{"arbitrary": "bad"}}}
	base, e := mutate.ConfigFile(empty.Image, cfg)
	if e != nil {
		t.Fatal(e)
	}
	return root, base
}
func TestAssembleCanonicalImageAndConfiguration(t *testing.T) {
	root, base := fixture(t)
	img, r, e := Assemble(context.Background(), base, root, identity)
	if e != nil {
		t.Fatal(e)
	}
	if len(r.Files) != 4 || r.Bytes == 0 || r.ImageDigest == "" || r.LayerDigest == "" {
		t.Fatalf("missing evidence: %+v", r)
	}
	cfg, e := img.ConfigFile()
	if e != nil {
		t.Fatal(e)
	}
	if cfg.Config.User != "185:185" || len(cfg.Config.Volumes) != 0 || cfg.Config.Entrypoint[0] != "/usr/lib/jvm/java-21/bin/java" || cfg.Config.Labels["arbitrary"] != "" || strings.Contains(strings.Join(cfg.Config.Env, " "), "UNTRUSTED") {
		t.Fatal("inherited unsafe configuration")
	}
	layers, e := img.Layers()
	if e != nil {
		t.Fatal(e)
	}
	rd, e := layers[len(layers)-1].Uncompressed()
	if e != nil {
		t.Fatal(e)
	}
	defer rd.Close()
	tr := tar.NewReader(rd)
	files := 0
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		if h.Uid != 185 || h.Gid != 185 || h.ModTime.Unix() != 0 || !strings.HasPrefix(h.Name, "deployments/") {
			t.Fatalf("bad archive header %+v", h)
		}
		if h.Typeflag == tar.TypeReg {
			files++
			if h.Mode != 0644 {
				t.Fatal("unreviewed executable mode")
			}
		}
	}
	if files != 4 {
		t.Fatal(files)
	}
	// File timestamps, writable/executable bits and unrelated build metadata must
	// not change the package or allow a source-controlled recipe to run.
	os.WriteFile(filepath.Join(root, "Dockerfile"), []byte("RUN false"), 0600)
	os.Chmod(filepath.Join(root, "quarkus-run.jar"), 0600)
	_, again, e := Assemble(context.Background(), base, root, identity)
	if e != nil || again.ImageDigest != r.ImageDigest {
		t.Fatalf("noncanonical image: %v", e)
	}
}
func TestAssembleRejectsUntrustedInput(t *testing.T) {
	cases := map[string]func(*testing.T, string){
		"symlink-file": func(t *testing.T, r string) {
			p := filepath.Join(r, "app/app.jar")
			os.Remove(p)
			os.Symlink("/etc/passwd", p)
		},
		"symlink-directory": func(t *testing.T, r string) {
			os.RemoveAll(filepath.Join(r, "lib"))
			os.Symlink("/tmp", filepath.Join(r, "lib"))
		},
		"hardlink": func(t *testing.T, r string) {
			os.Link(filepath.Join(r, "app/app.jar"), filepath.Join(r, "app/linked.jar"))
		},
		"fifo": func(t *testing.T, r string) {
			if e := unix.Mkfifo(filepath.Join(r, "app/pipe"), 0600); e != nil {
				t.Fatal(e)
			}
		},
		"socket": func(t *testing.T, r string) {
			if e := unix.Mknod(filepath.Join(r, "app/socket"), unix.S_IFSOCK|0600, 0); e != nil {
				t.Fatal(e)
			}
		},
		"whiteout":       func(t *testing.T, r string) { os.WriteFile(filepath.Join(r, "app/.wh.lib"), []byte("x"), 0600) },
		"empty-launcher": func(t *testing.T, r string) { os.Truncate(filepath.Join(r, "quarkus-run.jar"), 0) },
		"oversize":       func(t *testing.T, r string) { os.Truncate(filepath.Join(r, "quarkus-run.jar"), MaxFileBytes+1) },
		"missing-root":   func(t *testing.T, r string) { os.RemoveAll(filepath.Join(r, "quarkus")) },
		"file-root": func(t *testing.T, r string) {
			os.RemoveAll(filepath.Join(r, "lib"))
			os.WriteFile(filepath.Join(r, "lib"), []byte("x"), 0600)
		},
		"newline": func(t *testing.T, r string) { os.WriteFile(filepath.Join(r, "app/a\nb"), []byte("x"), 0600) },
		"depth":   func(t *testing.T, r string) { os.MkdirAll(filepath.Join(r, "app", strings.Repeat("a/", 26)), 0755) },
	}
	for name, alter := range cases {
		t.Run(name, func(t *testing.T) {
			r, b := fixture(t)
			alter(t, r)
			if _, _, e := Assemble(context.Background(), b, r, identity); e == nil {
				t.Fatal("accepted unsafe input")
			}
		})
	}
}
func TestAssembleRejectsBaseIdentityAndCanceledContext(t *testing.T) {
	r, b := fixture(t)
	for _, field := range []string{"onbuild", "platform"} {
		t.Run(field, func(t *testing.T) {
			c, _ := b.ConfigFile()
			if field == "onbuild" {
				c.Config.OnBuild = []string{"RUN bad"}
			} else {
				c.Architecture = "arm64"
			}
			bad, _ := mutate.ConfigFile(b, c)
			if _, _, e := Assemble(context.Background(), bad, r, identity); e == nil {
				t.Fatal("accepted base")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, e := Assemble(ctx, b, r, identity); e == nil {
		t.Fatal("ignored cancellation")
	}
	bad := identity
	bad.SourceSHA = "main"
	if _, _, e := Assemble(context.Background(), b, r, bad); e == nil {
		t.Fatal("accepted floating source")
	}
	link := filepath.Join(t.TempDir(), "input")
	os.Symlink(r, link)
	if _, _, e := Assemble(context.Background(), b, link, identity); e == nil {
		t.Fatal("accepted linked input root")
	}
}

func TestCaptureEnforcesAggregateBudgetsBeforeReading(t *testing.T) {
	for _, budget := range []string{"bytes", "files", "nodes"} {
		t.Run(budget, func(t *testing.T) {
			root, _ := fixture(t)
			f, e := openDirectory(root)
			if e != nil {
				t.Fatal(e)
			}
			defer f.Close()
			r := Report{}
			c := collector{ctx: context.Background(), tw: tar.NewWriter(io.Discard), report: &r}
			switch budget {
			case "bytes":
				r.Bytes = MaxBytes
			case "files":
				r.Files = make([]File, MaxFiles)
			case "nodes":
				c.nodes = MaxFiles + 1024
			}
			if e = c.entry(f, "quarkus-run.jar", "quarkus-run.jar", 0); e == nil {
				t.Fatal("accepted exhausted aggregate budget")
			}
		})
	}
}
