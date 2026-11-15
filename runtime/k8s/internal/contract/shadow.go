package contract

import (
	"bytes"
	"errors"
	"io"
	"os"

	"gopkg.in/yaml.v3"
)

// LoadShadow is the fail-closed policy entrypoint for the bounded shadow
// integration. Unknown fields, multiple documents and missing bounds reject
// startup; this supports the explicitly modeled contract subset only.
func LoadShadow(path, label string) (Contract, error) {
	var c Contract
	f, err := os.Open(path)
	if err != nil {
		return c, errors.New("shadow contract unavailable")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || len(raw) > 65536 {
		return c, errors.New("shadow contract exceeds bound")
	}
	d := yaml.NewDecoder(bytes.NewReader(raw))
	d.KnownFields(true)
	if d.Decode(&c) != nil || d.Decode(new(any)) != io.EOF {
		return Contract{}, errors.New("invalid shadow contract")
	}
	if c.Spec != "1.0.0" || c.Autonomy.Level != "shadow" || c.Triggers.IssueLabel != label || c.Triggers.MinBodyLength < 120 || c.Triggers.MinBodyLength > 16384 || c.Triggers.AcceptanceCriteria.Min != 1 || c.Triggers.AcceptanceCriteria.Max == nil || *c.Triggers.AcceptanceCriteria.Max < 1 || *c.Triggers.AcceptanceCriteria.Max > 8 {
		return Contract{}, errors.New("shadow contract requires explicit bounded admission policy")
	}
	return c, nil
}
