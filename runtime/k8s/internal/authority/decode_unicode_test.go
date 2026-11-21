package authority

import (
	"strings"
	"testing"
)

// encoding/json folds U+017F (long s) to "s" and U+212A (Kelvin) to "k" when
// matching field names; a non-ASCII key must never reach that folding.
func TestStrictDecodingRefusesUnicodeFoldedDuplicateKeys(t *testing.T) {
	type nested struct {
		Keys string `json:"keys"`
	}
	var v struct {
		Version string `json:"version"`
		Keys    string `json:"keys"`
		Nested  nested `json:"nested"`
	}
	// A JSON unicode escape introducer, built at run time so the source keeps
	// the escape rather than the decoded character.
	esc := string(rune(92)) + "u"
	for name, raw := range map[string]string{
		"literal long s":   "{\"version\":\"1\",\"verſion\":\"2\"}",
		"escaped long s":   `{"version":"1","ver` + esc + `017fion":"2"}`,
		"escaped kelvin":   `{"keys":"a","` + esc + `212aeys":"b"}`,
		"lone non-ASCII":   "{\"verſion\":\"2\"}",
		"nested duplicate": `{"nested":{"keys":"a","Keys":"b"}}`,
		"ASCII case":       `{"Version":"1","version":"2"}`,
	} {
		err := DecodeStrict([]byte(raw), &v)
		if err == nil {
			t.Fatal("ambiguous key accepted:", name, v)
		}
		if name != "ASCII case" && !strings.Contains(err.Error(), "non-ASCII") {
			t.Fatal("unexpected refusal reason:", name, err)
		}
	}
	if DecodeStrict([]byte(`{"version":"é","keys":"k","nested":{"keys":"n"}}`), &v) != nil || v.Version != "é" {
		t.Fatal("non-ASCII values must remain accepted")
	}
}
