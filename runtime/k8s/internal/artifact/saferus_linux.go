package artifact

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"golang.org/x/sys/unix"
)

const SaferusSourceURL = "https://github.com/os-santiago/saferus"
const SaferusRecipe = "ai-sdlc.saferus-go-static/v1"
const MaxSaferusBytes = 32 << 20

// AssembleSaferus never executes source, a Containerfile, or the resulting ELF.
// The controller must first establish that every producer terminated and mount
// the output read-only. ELF validation checks the runtime layout; it does not
// prove which compiler produced the bytes or establish application safety.
func AssembleSaferus(ctx context.Context, directory string, id Identity) (v1.Image, Report, error) {
	report := Report{Identity: id, Recipe: SaferusRecipe, Base: "scratch"}
	fail := func() (v1.Image, Report, error) { return nil, report, errors.New("Saferus static artifact rejected") }
	if id.Validate() != nil || ctx.Err() != nil {
		return fail()
	}
	root, err := openDirectory(directory)
	if err != nil {
		return fail()
	}
	defer root.Close()
	entries, err := root.ReadDir(2)
	if err != nil && err != io.EOF || len(entries) != 1 || entries[0].Name() != "saferus" {
		return fail()
	}
	fd, err := unix.Openat(int(root.Fd()), "saferus", unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return fail()
	}
	file := os.NewFile(uintptr(fd), "saferus")
	defer file.Close()
	var before, after unix.Stat_t
	if unix.Fstat(fd, &before) != nil || before.Mode&unix.S_IFMT != unix.S_IFREG || before.Nlink != 1 || before.Size < 64 || before.Size > MaxSaferusBytes || before.Mode&0o7777 != 0o755 {
		return fail()
	}
	raw, err := io.ReadAll(io.LimitReader(&contextReader{ctx, file}, MaxSaferusBytes+1))
	if err != nil || int64(len(raw)) != before.Size || unix.Fstat(fd, &after) != nil || before.Size != after.Size || before.Mtim != after.Mtim || before.Ctim != after.Ctim || before.Mode != after.Mode || before.Nlink != after.Nlink || ctx.Err() != nil {
		return fail()
	}
	if !staticLinuxAMD64(raw) {
		return fail()
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if tw.WriteHeader(&tar.Header{Name: "saferus", Mode: 0755, Uid: 1000, Gid: 1000, Size: int64(len(raw)), Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0), Format: tar.FormatUSTAR}) != nil {
		return fail()
	}
	if _, err = tw.Write(raw); err != nil {
		return fail()
	}
	if tw.Close() != nil {
		return fail()
	}
	sum := sha256.Sum256(raw)
	report.Bytes = int64(len(raw))
	report.Files = []File{{Path: "saferus", Bytes: int64(len(raw)), SHA256: hex.EncodeToString(sum[:])}}
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(buf.Bytes())), nil }, tarball.WithMediaType(types.OCILayer))
	if err != nil {
		return fail()
	}
	digest, err := layer.Digest()
	if err != nil {
		return fail()
	}
	report.LayerDigest = digest.String()
	base := mutate.ConfigMediaType(mutate.MediaType(empty.Image, types.OCIManifestSchema1), types.OCIConfigJSON)
	img, err := mutate.Append(base, mutate.Addendum{Layer: layer, History: v1.History{Created: v1.Time{Time: time.Unix(0, 0)}, CreatedBy: SaferusRecipe}})
	if err != nil {
		return fail()
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		return fail()
	}
	cfg.Created = v1.Time{Time: time.Unix(0, 0)}
	cfg.OS = "linux"
	cfg.Architecture = "amd64"
	cfg.Config = v1.Config{User: "1000:1000", WorkingDir: "/", Entrypoint: []string{"/saferus"}, ExposedPorts: map[string]struct{}{"8443/tcp": {}}, Labels: map[string]string{"org.opencontainers.image.source": SaferusSourceURL, "org.opencontainers.image.revision": id.SourceSHA, "io.ai-sdlc.recipe": SaferusRecipe, "io.ai-sdlc.definition": id.DefinitionDigest, "io.ai-sdlc.run-uid": id.RunUID}}
	img, err = mutate.ConfigFile(img, cfg)
	if err != nil {
		return fail()
	}
	digest, err = img.Digest()
	if err != nil || ctx.Err() != nil {
		return fail()
	}
	report.ImageDigest = digest.String()
	return img, report, nil
}

func staticLinuxAMD64(raw []byte) bool {
	f, err := elf.NewFile(bytes.NewReader(raw))
	if err != nil {
		return false
	}
	defer f.Close()
	if f.Class != elf.ELFCLASS64 || f.Data != elf.ELFDATA2LSB || f.Machine != elf.EM_X86_64 || f.Type != elf.ET_EXEC || (f.OSABI != elf.ELFOSABI_NONE && f.OSABI != elf.ELFOSABI_LINUX) || f.Entry == 0 {
		return false
	}
	executable := false
	for _, p := range f.Progs {
		if p.Off > uint64(len(raw)) || p.Filesz > uint64(len(raw))-p.Off || p.Filesz > p.Memsz {
			return false
		}
		if p.Type == elf.PT_INTERP || p.Type == elf.PT_DYNAMIC {
			return false
		}
		if p.Type == elf.PT_LOAD && p.Flags&elf.PF_X != 0 && p.Flags&elf.PF_W != 0 {
			return false
		}
		if p.Type == elf.PT_GNU_STACK && p.Flags&elf.PF_X != 0 {
			return false
		}
		if p.Type == elf.PT_LOAD && p.Flags&elf.PF_X != 0 && f.Entry >= p.Vaddr && f.Entry-p.Vaddr < p.Memsz {
			executable = true
		}
	}
	return executable
}
