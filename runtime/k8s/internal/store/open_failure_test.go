package store

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestOpenFailureClassesWithoutADatabase(t *testing.T) {
	_, err := Open(context.Background(), "::not a url")
	if OpenFailure(err) != OpenInvalidConfiguration {
		t.Fatal("unparsable URL", OpenFailure(err))
	}
	for code, want := range map[string]string{"28P01": OpenRefused, "28000": OpenRefused, "3D000": OpenRefused, "57P03": OpenUnavailable} {
		if got := OpenFailure(fmt.Errorf("ping database: %w", &pgconn.PgError{Code: code})); got != want {
			t.Errorf("%s: %s, want %s", code, got, want)
		}
	}
	// Nothing listens on port 1.
	_, err = Open(context.Background(), "postgres://reader:x@127.0.0.1:1/journal?sslmode=disable&connect_timeout=2")
	if OpenFailure(err) != OpenUnavailable {
		t.Fatal("unreachable server", OpenFailure(err))
	}
}

// A login the server refuses (here an unknown role, which PostgreSQL reports
// like a wrong password) is classified as refused.
func TestOpenFailureRefusedLogin(t *testing.T) {
	connection := os.Getenv("AI_SDLC_TEST_DATABASE_URL")
	if connection == "" {
		t.Skip("disposable PostgreSQL required")
	}
	u, err := url.Parse(connection)
	if err != nil {
		t.Fatal("test database URL")
	}
	u.User = url.UserPassword("aisdlc_role_that_does_not_exist", "wrong")
	_, err = Open(context.Background(), u.String())
	if OpenFailure(err) != OpenRefused {
		t.Fatal("refused login", OpenFailure(err))
	}
}
