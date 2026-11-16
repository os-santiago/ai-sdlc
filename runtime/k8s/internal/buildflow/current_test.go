package buildflow

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCurrentBuildRejectsStalePolicyCopy(t *testing.T) {
	i := installedFixture(t)
	dir := t.TempDir()
	installation, inner, outer := filepath.Join(dir, "installation.json"), filepath.Join(dir, "inner.json"), filepath.Join(dir, "outer.json")
	write := func(path string, v any) {
		raw, _ := json.Marshal(v)
		if e := os.WriteFile(path, raw, 0600); e != nil {
			t.Fatal(e)
		}
	}
	write(installation, i)
	write(inner, i.Inner)
	write(outer, i.Outer)
	if _, e := LoadCurrent(installation, inner, outer); e != nil {
		t.Fatal(e)
	}
	i.Outer.Freeze = !i.Outer.Freeze
	write(outer, i.Outer)
	if _, e := LoadCurrent(installation, inner, outer); e == nil {
		t.Fatal("stale installed policy overrode canonical freeze")
	}
	write(installation, i)
	if _, e := LoadCurrent(installation, inner, outer); e != nil {
		t.Fatal("consistent freeze rejected", e)
	}
	if e := os.Remove(inner); e != nil {
		t.Fatal(e)
	}
	if _, e := LoadCurrent(installation, inner, outer); e == nil {
		t.Fatal("missing canonical policy accepted")
	}
	if _, e := LoadCurrent(installation, outer, outer); e == nil {
		t.Fatal("same document supplied for both loops")
	}
}
