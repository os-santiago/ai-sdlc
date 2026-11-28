package project

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

// Dependencies declares a read-only dependency cache for the offline
// verifier. Tests never get a network. A trusted dependency task (the
// maven-dependencies Tekton Task, run only on the base revision with the
// dependency proxy as its sole reachable host) fills a cache volume, which an
// operator seals into the verifier namespace; every verification pod then
// mounts it read-only at CachePath. The section is optional and omitted when
// absent, so manifests without it keep their canonical form, digest and
// verifier settings.
//
// The cache is keyed by DependencyKey: the image, this section and the
// contents of the files KeyGlobs match in the base tree. A candidate that
// changes one of those files is refused as a harness change, so the code
// that ran with network access was always reviewed base code.
type Dependencies struct {
	// CachePath is where the sealed cache is mounted read-only (for Maven
	// 3.9 the tests use -Dmaven.repo.local.tail=CachePath with a writable
	// local repository under /tmp).
	CachePath string `json:"cachePath"`
	// KeyGlobs match the files that define the dependency set (for Maven:
	// every pom.xml, .mvn/** and the wrapper scripts; for npm: package.json,
	// package-lock.json and .npmrc). They are part of the test harness: a
	// candidate that changes one is refused.
	KeyGlobs []string `json:"keyGlobs"`
}

// Bounds of Dependencies.
const (
	MaxDependencyKeyGlobs            = 32
	DependencyKeyVersion             = "ai-sdlc.dependencies/v1"
	dependencyClaimPrefix            = "deps-"
	maxDependencyCachePathComponents = 16
)

var (
	dependencyClaim = regexp.MustCompile(`^deps-[a-f0-9]{64}$`)
	digestHex       = regexp.MustCompile(`^[a-f0-9]{64}$`)
	// dependencyExcluded are directories the cache may not be mounted at or
	// under: a toolchain root's exclusions (the run's writable volumes,
	// settings, identity, kernel and device filesystems) and the system
	// directories that hold the toolchain's programs, which a mount would
	// hide.
	dependencyExcluded = append(append([]string{}, toolchainExcluded...), "/usr/bin", "/bin", "/sbin", "/usr/sbin", "/lib", "/lib64", "/usr/lib")
)

func (d Dependencies) validate() error {
	if validCachePath(d.CachePath) != nil || len(d.KeyGlobs) == 0 || len(d.KeyGlobs) > MaxDependencyKeyGlobs {
		return ErrManifest
	}
	seen := map[string]bool{}
	for _, g := range d.KeyGlobs {
		if validGlob(g) != nil || seen[g] {
			return ErrManifest
		}
		seen[g] = true
	}
	return nil
}

// validCachePath accepts an absolute, clean path of at most 16 components in
// the conservative character set, outside the excluded directories.
func validCachePath(p string) error {
	if ValidateToolchainRoot(p) != nil || strings.Count(p, "/") > maxDependencyCachePathComponents {
		return ErrManifest
	}
	for _, excluded := range dependencyExcluded {
		// The cache may not hide an excluded directory either: /usr would
		// shadow /usr/bin and /usr/lib.
		if p == excluded || strings.HasPrefix(p, excluded+"/") || strings.HasPrefix(excluded, p+"/") {
			return ErrManifest
		}
	}
	return nil
}

// MatchesDependencies reports whether a path defines the dependency set.
func (v Verification) MatchesDependencies(file string) bool {
	if v.Dependencies == nil || cleanPath(file) != nil {
		return false
	}
	for _, g := range v.Dependencies.KeyGlobs {
		if matchGlob(g, file) {
			return true
		}
	}
	return false
}

// DependencyFile is one file of a source tree, as DependencyKey reads it.
type DependencyFile struct {
	Path string
	Data []byte
}

// DependencyKey identifies a dependency cache. It is the sha256 of a text
// document that a shell or Python can reproduce:
//
//	ai-sdlc.dependencies/v1\n
//	image <verification image>\n
//	settings <sha256 of the canonical dependencies section>\n
//	<sha256 of the file>  <path>\n   (one line per KeyGlobs file, sorted by path)
//
// Paths must be clean and unique. ok is false without a dependencies section.
func (v Verification) DependencyKey(files []DependencyFile) (string, bool) {
	if v.Dependencies == nil || v.Validate() != nil {
		return "", false
	}
	var settings bytes.Buffer
	encoder := json.NewEncoder(&settings)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(v.Dependencies) != nil {
		return "", false
	}
	matched := []DependencyFile{}
	seen := map[string]bool{}
	for _, f := range files {
		if cleanPath(f.Path) != nil || seen[f.Path] {
			return "", false
		}
		seen[f.Path] = true
		if v.MatchesDependencies(f.Path) {
			matched = append(matched, f)
		}
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].Path < matched[j].Path })
	var document strings.Builder
	document.WriteString(DependencyKeyVersion + "\nimage " + v.Image + "\nsettings " + hexDigest(bytes.TrimSuffix(settings.Bytes(), []byte("\n"))) + "\n")
	for _, f := range matched {
		document.WriteString(hexDigest(f.Data) + "  " + f.Path + "\n")
	}
	return hexDigest([]byte(document.String())), true
}

// DependencyClaimName is the name of the sealed cache volume claim for a key
// in the verifier namespace, or "" for an invalid key.
func DependencyClaimName(key string) string {
	if !digestHex.MatchString(key) {
		return ""
	}
	return dependencyClaimPrefix + key
}

// ValidDependencyClaimName reports whether name is a DependencyClaimName.
func ValidDependencyClaimName(name string) bool { return dependencyClaim.MatchString(name) }

func hexDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
