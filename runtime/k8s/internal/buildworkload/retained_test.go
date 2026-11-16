package buildworkload

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
)

func TestRetainedContextReconstructsHistoricalDefinitionWithoutAuthority(t *testing.T) {
	cfg, p, c, key := setup(t)
	c.IssuedAt = time.Now().Add(-time.Hour).UnixMilli()
	c.ExpiresAt = c.IssuedAt + building.MaxAge.Milliseconds()
	raw, _ := json.Marshal(c)
	signed, e := key.Seal(cfg.Outer, c.ID(), "build-merged", raw)
	if e != nil {
		t.Fatal(e)
	}
	w, e := Compile(cfg, p, signed, raw, cfg.Slots[0], time.UnixMilli(c.IssuedAt))
	if e != nil {
		t.Fatal(e)
	}
	if e = w.Retained.ValidateHistorical(c, w.Plan); e != nil {
		t.Fatal(e)
	}
	if _, e = Compile(cfg, p, signed, raw, cfg.Slots[0], time.Now()); e == nil {
		t.Fatal("history renewed execution authority")
	}
	encoded, e := w.Retained.Encode()
	if e != nil {
		t.Fatal(e)
	}
	decoded, e := DecodeRetained(encoded)
	if e != nil || decoded.ValidateHistorical(c, w.Plan) != nil {
		t.Fatal("roundtrip", e)
	}
	// Caller mutation cannot change the recorded value or compiled request bytes.
	cfg.Slots[0].Node = "changed"
	p.Dependencies[required[0]] = Dependency{}
	if decoded.ValidateHistorical(c, w.Plan) != nil || w.Retained.ValidateHistorical(c, w.Plan) != nil {
		t.Fatal("aliased installation inputs")
	}
	w.Build[0] = 'X'
	w.Retained.Build[0] = 'Y'
	decoded.Profile.Dependencies[required[0]] = Dependency{}
	reread, e := DecodeRetained(encoded)
	if e != nil || reread.ValidateHistorical(c, w.Plan) != nil {
		t.Fatal("decoded historical value aliased another copy")
	}
}

func TestRetainedContextRejectsSelfConsistentSubstitution(t *testing.T) {
	for _, name := range []string{"version", "compiler", "recipe", "binding-version", "freshness", "unselected-slot", "slot-order", "intake", "selected-slot", "profile", "source", "build-bytes", "publish-bytes", "self-hashed-bytes", "oversized", "missing"} {
		t.Run(name, func(t *testing.T) {
			cfg, p, c, key := setup(t)
			other := cfg.Slots[0]
			other.PVC = "build-slot-1"
			other.PVCUID = "22222222-2222-2222-2222-222222222222"
			other.PVUID = "33333333-3333-3333-3333-333333333333"
			other.StorageIdentitySHA256 = strings.Repeat("f", 64)
			cfg.Slots = append(cfg.Slots, other)
			c.ProfileSHA256 = cfg.ProfileDigest()
			w, e := compile(t, cfg, p, c, key)
			if e != nil {
				t.Fatal(e)
			}
			r := w.Retained
			plan := w.Plan
			switch name {
			case "version":
				r.Version = "future"
			case "compiler":
				r.Compiler = "arbitrary-plugin"
			case "recipe":
				r.Recipe = "arbitrary-script"
			case "binding-version":
				r.Configuration.Version = "future"
			case "freshness":
				r.Configuration.IssueMaxAgeSeconds++
			case "unselected-slot":
				r.Configuration.Slots[1].Node = "replacement"
			case "slot-order":
				r.Configuration.Slots[0], r.Configuration.Slots[1] = r.Configuration.Slots[1], r.Configuration.Slots[0]
			case "intake":
				r.IntakeConfigSHA256 = strings.Repeat("a", 64)
			case "selected-slot":
				r.Slot = other
			case "profile":
				r.Profile.Dependencies[required[0]] = Dependency{other.PVCUID, strings.Repeat("f", 64)}
			case "source":
				c.SourceSHA = strings.Repeat("0", 40)
			case "build-bytes":
				r.Build = append(r.Build, ' ')
			case "publish-bytes":
				r.Publish = append(r.Publish, ' ')
			case "self-hashed-bytes":
				r.Build = append(r.Build, ' ')
				plan.BuildRequestSHA256 = authority.Digest(r.Build)
			case "oversized":
				r.Publish = bytes.Repeat([]byte{'x'}, 32769)
			case "missing":
				r.Build = nil
			}
			if r.ValidateHistorical(c, plan) == nil {
				t.Fatal("substitution accepted")
			}
		})
	}
}

func TestRetainedContextStrictEnvelope(t *testing.T) {
	cfg, p, c, key := setup(t)
	w, e := compile(t, cfg, p, c, key)
	if e != nil {
		t.Fatal(e)
	}
	raw, e := w.Retained.Encode()
	if e != nil {
		t.Fatal(e)
	}
	for _, bad := range [][]byte{nil, bytes.Repeat([]byte{' '}, MaxRetainedBytes+1), append(append([]byte{}, raw...), raw...), bytes.Replace(raw, []byte(`"version":`), []byte(`"Version":`), 1), bytes.Replace(raw, []byte(`"version":`), []byte(`"extra":true,"version":`), 1), bytes.Replace(raw, []byte(`"version":`), []byte(`"version":"duplicate","version":`), 1)} {
		if _, e := DecodeRetained(bad); e == nil {
			t.Fatal("ambiguous or excessive envelope accepted")
		}
	}
}
