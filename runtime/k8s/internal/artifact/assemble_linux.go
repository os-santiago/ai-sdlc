// Package artifact assembles a reviewed Quarkus JVM layout without executing
// source-controlled build instructions. Callers must terminate all input writers
// and mount the input read-only before invoking Assemble.
package artifact

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"golang.org/x/sys/unix"
)

const (
	Base         = "registry.access.redhat.com/ubi9/openjdk-21@sha256:4daa3f2fa0f2f64cc7732dca63ef1ed7c53ff4883cc18571d6f8eca7a8458b46"
	SourceURL    = "https://github.com/os-santiago/homedir"
	Recipe       = "ai-sdlc.quarkus-jvm/v1"
	MaxFiles     = 4096
	MaxBytes     = 256 << 20
	MaxFileBytes = 64 << 20
)

var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var uidPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]{0,127}$`)

type Identity struct {
	SourceSHA        string `json:"sourceSHA"`
	DefinitionDigest string `json:"definitionDigest"`
	RunUID           string `json:"runUID"`
}

func (i Identity) Validate() error {
	if !shaPattern.MatchString(i.SourceSHA) || !digestPattern.MatchString(i.DefinitionDigest) || !uidPattern.MatchString(i.RunUID) {
		return errors.New("invalid trusted build identity")
	}
	return nil
}

type File struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}
type Report struct {
	Identity
	Recipe      string `json:"recipe"`
	Base        string `json:"base"`
	Files       []File `json:"files"`
	Bytes       int64  `json:"bytes"`
	LayerDigest string `json:"layerDigest"`
	ImageDigest string `json:"imageDigest"`
}

// openDirectory rejects links in every ancestor, including the root path.
func openDirectory(p string) (*os.File, error) {
	if !strings.HasPrefix(p, "/") || path.Clean(p) != p {
		return nil, errors.New("input directory must be canonical absolute path")
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(p, "/"), "/") {
		if part == "" {
			continue
		}
		next, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if e != nil {
			return nil, errors.New("input ancestor is not a regular directory")
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), p), nil
}

type collector struct {
	ctx    context.Context
	tw     *tar.Writer
	report *Report
	nodes  int
}

func (c *collector) entry(parent *os.File, name, rel string, depth int) error {
	if err := c.ctx.Err(); err != nil {
		return err
	}
	c.nodes++
	if c.nodes > MaxFiles+1024 || depth > 24 || len(rel) > 1024 || strings.ContainsAny(rel, "\x00\n\r\\") {
		return errors.New("artifact path budget exceeded")
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return errors.New("artifact entry cannot be safely opened")
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	var st unix.Stat_t
	if err = unix.Fstat(fd, &st); err != nil {
		return err
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
		if depth == 0 && name == "quarkus-run.jar" {
			return errors.New("launcher must be a regular file")
		}
		if err = c.tw.WriteHeader(&tar.Header{Name: "deployments/" + rel + "/", Mode: 0755, Uid: 185, Gid: 185, Typeflag: tar.TypeDir, ModTime: time.Unix(0, 0)}); err != nil {
			return err
		}
		names, err := f.Readdirnames(MaxFiles + 1025)
		if err != nil && err != io.EOF {
			return err
		}
		sort.Strings(names)
		for _, child := range names {
			if strings.HasPrefix(child, ".wh.") {
				return errors.New("OCI whiteout names are forbidden")
			}
			if err = c.entry(f, child, path.Join(rel, child), depth+1); err != nil {
				return err
			}
		}
	case unix.S_IFREG:
		if depth == 0 && name != "quarkus-run.jar" {
			return errors.New("expected artifact directory")
		}
		if st.Nlink != 1 || st.Size < 1 || st.Size > MaxFileBytes || c.report.Bytes+st.Size > MaxBytes || len(c.report.Files) >= MaxFiles {
			return errors.New("artifact regular-file budget or link constraint violated")
		}
		if err = c.tw.WriteHeader(&tar.Header{Name: "deployments/" + rel, Mode: 0644, Uid: 185, Gid: 185, Typeflag: tar.TypeReg, Size: st.Size, ModTime: time.Unix(0, 0)}); err != nil {
			return err
		}
		h := sha256.New()
		n, err := io.CopyN(io.MultiWriter(c.tw, h), &contextReader{c.ctx, f}, st.Size)
		if err != nil {
			return err
		}
		var extra [1]byte
		if m, e := f.Read(extra[:]); m != 0 || e != io.EOF {
			return errors.New("artifact changed during capture")
		}
		var after unix.Stat_t
		if err = unix.Fstat(fd, &after); err != nil {
			return err
		}
		if after.Size != st.Size || after.Mtim != st.Mtim || after.Ctim != st.Ctim {
			return errors.New("artifact metadata changed during capture")
		}
		c.report.Bytes += n
		c.report.Files = append(c.report.Files, File{rel, n, hex.EncodeToString(h.Sum(nil))})
	default:
		return errors.New("artifact contains a special node")
	}
	return nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	return r.r.Read(p)
}

// Assemble accepts exactly four reviewed package roots. Other Maven outputs are
// not interpreted as instructions. It replaces all inherited image configuration.
func Assemble(ctx context.Context, base v1.Image, directory string, id Identity) (v1.Image, Report, error) {
	report := Report{Identity: id, Recipe: Recipe}
	if err := id.Validate(); err != nil {
		return nil, report, err
	}
	observedBase, err := base.Digest()
	if err != nil {
		return nil, report, err
	}
	report.Base = observedBase.String()
	config, err := base.ConfigFile()
	if err != nil {
		return nil, report, err
	}
	if len(config.Config.OnBuild) != 0 || config.OS != "linux" || config.Architecture != "amd64" {
		return nil, report, errors.New("base has an unreviewed platform or ONBUILD trigger")
	}
	root, err := openDirectory(directory)
	if err != nil {
		return nil, report, err
	}
	defer root.Close()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err = tw.WriteHeader(&tar.Header{Name: "deployments/", Mode: 0755, Uid: 185, Gid: 185, Typeflag: tar.TypeDir, ModTime: time.Unix(0, 0)}); err != nil {
		return nil, report, err
	}
	c := collector{ctx: ctx, tw: tw, report: &report}
	for _, p := range []string{"app", "lib", "quarkus", "quarkus-run.jar"} {
		if err = c.entry(root, p, p, 0); err != nil {
			return nil, report, fmt.Errorf("capture package: %w", err)
		}
	}
	if err = tw.Close(); err != nil {
		return nil, report, err
	}
	if err = ctx.Err(); err != nil {
		return nil, report, err
	}
	manifestType, err := base.MediaType()
	if err != nil {
		return nil, report, err
	}
	layerType := types.DockerLayer
	switch manifestType {
	case types.OCIManifestSchema1:
		layerType = types.OCILayer
	case types.DockerManifestSchema2:
	default:
		return nil, report, errors.New("unsupported base manifest media type")
	}
	// Docker schema 2 rejects OCI uncompressed layer descriptors. Preserve the
	// base format and gzip through a reusable in-memory opener (no writable tmp).
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(buf.Bytes())), nil
	}, tarball.WithMediaType(layerType))
	if err != nil {
		return nil, report, err
	}
	digest, err := layer.Digest()
	if err != nil {
		return nil, report, err
	}
	report.LayerDigest = digest.String()
	img, err := mutate.Append(base, mutate.Addendum{Layer: layer, History: v1.History{Created: v1.Time{Time: time.Unix(0, 0)}, CreatedBy: Recipe}})
	if err != nil {
		return nil, report, err
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		return nil, report, err
	}
	cfg.Created = v1.Time{Time: time.Unix(0, 0)}
	cfg.Author = ""
	cfg.Container = ""
	cfg.DockerVersion = ""
	cfg.Config = v1.Config{User: "185:185", WorkingDir: "/deployments", Env: []string{"LANG=C.utf8", "HOME=/tmp", "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "JAVA_HOME=/usr/lib/jvm/java-21"}, Entrypoint: []string{"/usr/lib/jvm/java-21/bin/java"}, Cmd: []string{"-Dquarkus.http.host=0.0.0.0", "-Djava.util.logging.manager=org.jboss.logmanager.LogManager", "-jar", "/deployments/quarkus-run.jar"}, ExposedPorts: map[string]struct{}{"8080/tcp": {}}, Labels: map[string]string{"org.opencontainers.image.source": SourceURL, "org.opencontainers.image.revision": id.SourceSHA, "io.ai-sdlc.recipe": Recipe, "io.ai-sdlc.definition": id.DefinitionDigest, "io.ai-sdlc.run-uid": id.RunUID}}
	img, err = mutate.ConfigFile(img, cfg)
	if err != nil {
		return nil, report, err
	}
	digest, err = img.Digest()
	if err != nil {
		return nil, report, err
	}
	report.ImageDigest = digest.String()
	return img, report, nil
}
