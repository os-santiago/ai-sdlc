package sourcefetch

import (
	"context"
	"strings"
	"testing"
)

func TestExplicitSourceCredentialScope(t *testing.T) {
	token := strings.Repeat("x", 32)
	for _, in := range []struct{ repo, token string }{{"bad", token}, {"fixture/product", ""}, {"fixture/product", "bad\ncredential"}} {
		if _, err := WithReadCredential(context.Background(), in.repo, in.token); err == nil {
			t.Fatal("invalid credential accepted")
		}
	}
	if _, err := WithReadCredential(nil, "fixture/product", token); err == nil {
		t.Fatal("nil context accepted")
	}
	ctx, err := WithReadCredential(context.Background(), "fixture/product", token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = WithReadCredential(ctx, "fixture/product", token); err == nil {
		t.Fatal("nested authority accepted")
	}
	client, _ := New("fixture/product", "")
	t.Setenv("AI_SDLC_TEST_SOURCE_TOKEN", token)
	ordinary, err := client.scoped(context.Background())
	if err != nil || ordinary != client || ordinary.token != "" {
		t.Fatal("ambient credential used")
	}
	bound, err := client.scoped(ctx)
	if err != nil || bound == client || bound.token != token || client.token != "" {
		t.Fatal("credential escaped call scope")
	}
}

func TestSourceTokenIsOpaqueAndHeaderBounded(t *testing.T) {
	for _, token := range []string{strings.Repeat("x", 32), "ghs_15368_" + strings.Repeat("a", 240) + "." + strings.Repeat("b", 240) + "." + strings.Repeat("c", 86), strings.Repeat("x", 4096)} {
		if _, err := WithReadCredential(context.Background(), "fixture/product", token); err != nil {
			t.Fatal("valid opaque shape rejected")
		}
	}
	for _, token := range []string{strings.Repeat("x", 4097), strings.Repeat("x", 32) + "\r\nInjected: yes", strings.Repeat("x", 32) + " ", strings.Repeat("x", 32) + "\x00"} {
		if _, err := WithReadCredential(context.Background(), "fixture/product", token); err == nil {
			t.Fatal("unsafe header accepted")
		}
	}
}
