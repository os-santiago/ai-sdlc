package candidate

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

func sourceFixture() workspacebundle.Bundle {
	return workspacebundle.Bundle{Version: workspacebundle.Version, SourceSHA: strings.Repeat("a", 40), Files: []workspacebundle.File{
		{Path: "unchanged", Mode: 0644, Data: []byte("keep")},
		{Path: "modified", Mode: 0644, Data: []byte("old")},
		{Path: "deleted", Mode: 0644, Data: []byte("remove")},
		{Path: "mode", Mode: 0644, Data: []byte("same")},
	}}
}
func resultFixture() workspacebundle.Bundle {
	b := sourceFixture()
	b.Files = []workspacebundle.File{{Path: "mode", Mode: 0755, Data: []byte("same")}, {Path: "unchanged", Mode: 0644, Data: []byte("keep")}, {Path: "modified", Mode: 0644, Data: []byte("new")}, {Path: "created", Mode: 0644, Data: []byte{}}}
	return b
}
func TestDeltaReconstructsAllOperationsAndOwnsBytes(t *testing.T) {
	source, result := sourceFixture(), resultFixture()
	d, err := Build(source, result)
	if err != nil || len(d.Changes) != 4 {
		t.Fatal(err)
	}
	raw, err := Encode(d)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := Apply(source, decoded)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := result.TreeDigest()
	got, _ := rebuilt.TreeDigest()
	if want != got {
		t.Fatal("tree mismatch")
	}
	result.Files[0].Data[0] ^= 1
	for _, c := range d.Changes {
		if c.Path == "mode" && string(c.After.Data) != "same" {
			t.Fatal("caller mutated delta")
		}
	}
	rebuilt.Files[len(rebuilt.Files)-1].Data[0] ^= 1
	if string(source.Files[0].Data) != "keep" {
		t.Fatal("reconstruction aliases source")
	}
}
func TestDeltaRejectsFalsePreimagesAndUnsafeResults(t *testing.T) {
	d, _ := Build(sourceFixture(), resultFixture())
	original, _ := Encode(d)
	for _, mode := range []string{"source", "base-tree", "result-tree", "preimage", "mode", "duplicate", "unsorted", "noop", "create-existing", "delete-absent", "path", "after-path", "parent-file"} {
		t.Run(mode, func(t *testing.T) {
			value, _ := Decode(original)
			source := sourceFixture()
			switch mode {
			case "source":
				value.SourceSHA = strings.Repeat("b", 40)
			case "base-tree":
				value.SourceTreeSHA256 = strings.Repeat("b", 64)
			case "result-tree":
				value.CandidateTreeSHA256 = strings.Repeat("b", 64)
			case "preimage":
				value.Changes[1].Before.SHA256 = strings.Repeat("b", 64)
			case "mode":
				value.Changes[1].Before.Mode = 0755
			case "duplicate":
				value.Changes = append(value.Changes, value.Changes[0])
			case "unsorted":
				value.Changes[0], value.Changes[1] = value.Changes[1], value.Changes[0]
			case "noop":
				id := identity(*value.Changes[0].After)
				value.Changes[0].Before = &id
			case "create-existing":
				value.Changes[2].Before = nil
			case "delete-absent":
				value.Changes[0].After = nil
				id := identity(source.Files[0])
				value.Changes[0].Before = &id
			case "path":
				value.Changes[0].Path = "../outside"
			case "after-path":
				value.Changes[0].After.Path = "elsewhere"
			case "parent-file":
				value.Changes[0].Path = "unchanged/child"
				value.Changes[0].After.Path = value.Changes[0].Path
			}
			if _, err := Apply(source, value); err == nil {
				t.Fatal("invalid reconstruction accepted")
			}
		})
	}
}
func TestDeltaRejectsEmptyOversizedAndAmbiguousEncodings(t *testing.T) {
	if _, err := Build(sourceFixture(), sourceFixture()); err == nil {
		t.Fatal("no-op captured")
	}
	result := sourceFixture()
	for i := 0; i < MaxChanges+1; i++ {
		result.Files = append(result.Files, workspacebundle.File{Path: fmt.Sprintf("new-%03d", i), Mode: 0644, Data: []byte("x")})
	}
	if _, err := Build(sourceFixture(), result); err == nil {
		t.Fatal("change limit")
	}
	result = resultFixture()
	result.Files[0].Data = bytes.Repeat([]byte("x"), MaxBytes)
	if _, err := Build(sourceFixture(), result); err == nil {
		t.Fatal("serialized limit")
	}
	d, _ := Build(sourceFixture(), resultFixture())
	raw, _ := Encode(d)
	for _, bad := range [][]byte{append(raw, []byte("{}")...), append([]byte(" "), raw...), bytes.Replace(raw, []byte(`"version":`), []byte(`"extra":1,"version":`), 1), bytes.Replace(raw, []byte(`"version":`), []byte(`"version":"duplicate","version":`), 1)} {
		if _, err := Decode(bad); err == nil {
			t.Fatal("ambiguous/noncanonical delta")
		}
	}
}
