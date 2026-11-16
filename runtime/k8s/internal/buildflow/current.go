package buildflow

import (
	"io"
	"os"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
)

// LoadCurrent requires the installed snapshot to match independently mounted
// canonical policy documents. A stale snapshot cannot extend an earlier grant
// or override a freeze. Missing/corrupt policy never selects cached authority.
func LoadCurrent(installation, innerPath, outerPath string) (Installed, error) {
	i, e := LoadInstalled(installation)
	if e != nil {
		return Installed{}, e
	}
	for path, expected := range map[string]authority.Policy{innerPath: i.Inner, outerPath: i.Outer} {
		if path == "" || innerPath == outerPath {
			return Installed{}, ErrBuildFlow
		}
		f, e := os.Open(path)
		if e != nil {
			return Installed{}, ErrBuildFlow
		}
		raw, readErr := io.ReadAll(io.LimitReader(f, 128*1024+1))
		closeErr := f.Close()
		if readErr != nil || closeErr != nil || len(raw) > 128*1024 {
			return Installed{}, ErrBuildFlow
		}
		actual, e := authority.ParsePolicy(raw)
		if e != nil || actual.Revision() != expected.Revision() {
			return Installed{}, ErrBuildFlow
		}
	}
	return i, nil
}
