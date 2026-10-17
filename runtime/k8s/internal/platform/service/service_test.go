package service

import (
	"testing"
	"time"
)

func TestMetadataRequiresNameAndResponsibility(t *testing.T) {
	tests := []Metadata{
		{},
		{Name: "worker"},
		{Responsibility: "implementation"},
	}
	for _, metadata := range tests {
		if err := Run(metadata); err == nil {
			t.Fatalf("expected invalid metadata to fail: %#v", metadata)
		}
	}
}

func TestValueOrDefault(t *testing.T) {
	t.Setenv("AI_SDLC_TEST_VALUE", "configured")
	if got := valueOrDefault("AI_SDLC_TEST_VALUE", "fallback"); got != "configured" {
		t.Fatalf("got %q", got)
	}
	if got := valueOrDefault("AI_SDLC_MISSING_VALUE", "fallback"); got != "fallback" {
		t.Fatalf("got %q", got)
	}
}

func TestDurationOrDefault(t *testing.T) {
	t.Setenv("AI_SDLC_TEST_DURATION", "90s")
	if got := durationOrDefault("AI_SDLC_TEST_DURATION", time.Minute); got != 90*time.Second {
		t.Fatalf("got %v, want 90s", got)
	}
	if got := durationOrDefault("AI_SDLC_MISSING_DURATION", time.Minute); got != time.Minute {
		t.Fatalf("got %v, want fallback of 1m", got)
	}
}

func TestDurationOrDefaultFallsBackOnUnparsableValue(t *testing.T) {
	// A misconfigured timeout shouldn't keep the component from starting
	// at all — fall back rather than erroring.
	t.Setenv("AI_SDLC_TEST_DURATION", "not-a-duration")
	if got := durationOrDefault("AI_SDLC_TEST_DURATION", time.Minute); got != time.Minute {
		t.Fatalf("got %v, want fallback of 1m for an unparsable value", got)
	}
}
