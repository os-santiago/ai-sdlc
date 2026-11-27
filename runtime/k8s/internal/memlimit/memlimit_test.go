package memlimit

import (
	"errors"
	"testing"
)

func TestFromCgroup(t *testing.T) {
	files := func(m map[string]string) func(string) ([]byte, error) {
		return func(name string) ([]byte, error) {
			if v, ok := m[name]; ok {
				return []byte(v), nil
			}
			return nil, errors.New("absent")
		}
	}
	gib := int64(1 << 30)
	for name, c := range map[string]struct {
		files map[string]string
		want  int64
	}{
		"v2":          {map[string]string{limitFiles[0]: "1073741824\n"}, int64(float64(gib) * Fraction)},
		"v2-max":      {map[string]string{limitFiles[0]: "max\n", limitFiles[1]: "1073741824"}, 0},
		"v1":          {map[string]string{limitFiles[1]: "1073741824\n"}, int64(float64(gib) * Fraction)},
		"v1-no-limit": {map[string]string{limitFiles[1]: "9223372036854771712\n"}, 0},
		"none":        {map[string]string{}, 0},
		"garbage":     {map[string]string{limitFiles[0]: "-1"}, 0},
	} {
		if got := fromCgroup(files(c.files)); got != c.want {
			t.Fatalf("%s: got %d want %d", name, got, c.want)
		}
	}
}
