package authority

import "testing"

// encoding/json folds U+017F (long s) to "s" and U+212A (Kelvin) to "k" when
// matching field names; a non-ASCII key must never reach that folding.
func TestStrictDecodingRefusesUnicodeFoldedDuplicateKeys(t *testing.T) {
	var v struct {
		Version string `json:"version"`
		Keys    string `json:"keys"`
	}
	for _, raw := range []string{
		"{\"version\":\"1\",\"verſion\":\"2\"}",
		`{"version":"1","verſion":"2"}`,
		"{\"keys\":\"a\",\"Keys\":\"b\"}",
		`{"Version":"1","version":"2"}`,
	} {
		if DecodeStrict([]byte(raw), &v) == nil {
			t.Fatal("ambiguous key accepted", raw, v)
		}
	}
	if DecodeStrict([]byte(`{"version":"é","keys":"k"}`), &v) != nil || v.Version != "é" {
		t.Fatal("non-ASCII values must remain accepted")
	}
}
