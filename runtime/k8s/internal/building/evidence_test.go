package building

import (
	"strings"
	"testing"
)

func TestPublisherImageRequiresExactHostname(t *testing.T) {
	for _, host := range []string{"quayXio", "quay-io", "quay_io", "evil.quay.io", "quay.io.evil"} {
		if publisherImage.MatchString(host + "/os-santiago/ai-sdlc@sha256:" + strings.Repeat("a", 64)) {
			t.Fatal("foreign registry accepted", host)
		}
	}
	for _, repository := range []string{"quay.io/os-santiago/ai-sdlc", "localhost/ai-sdlc-artifact-builder"} {
		if !publisherImage.MatchString(repository + "@sha256:" + strings.Repeat("a", 64)) {
			t.Fatal("reviewed publisher rejected")
		}
	}
}
