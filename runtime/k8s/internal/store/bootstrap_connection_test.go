package store

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestBootstrapConnectionDiagnosticsNeverExposeDriverMaterial(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{pgconn.NewParseConfigError("postgres://private:secret@private/db", "private-sentinel", nil), "connection-configuration"},
		{&pgconn.PgError{Code: "28P01", Message: "private-sentinel", Detail: "secret"}, "connection-authentication"},
		{&pgconn.PgError{Code: "3D000", Message: "private-sentinel"}, "connection-database"},
		{&pgconn.PgError{Code: "42501", Message: "private-sentinel"}, "connection-permission"},
		{&pgconn.PgError{Code: "53300", Message: "private-sentinel"}, "connection-capacity"},
		{&pgconn.PgError{Code: "57P03", Message: "private-sentinel"}, "connection-unavailable"},
		{&pgconn.PgError{Code: "private-sentinel", Message: "secret"}, "connection"},
		{context.DeadlineExceeded, "connection-timeout"},
		{&net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, "connection-network"},
		{syscall.EACCES, "connection-permission"},
		{errors.New("postgres://private:secret@private/db"), "connection"},
	}
	for _, tc := range cases {
		got := BootstrapConnectionBoundary(fmt.Errorf("private-wrapper: %w", tc.err))
		if got != tc.want || strings.Contains(got, "private") || strings.Contains(got, "secret") {
			t.Fatal("unsafe connection classification")
		}
	}
}

func TestBootstrapConnectionRealMissingDatabase(t *testing.T) {
	raw := os.Getenv("AI_SDLC_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("requires disposable PostgreSQL fixture")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal("fixture URL unavailable")
	}
	parsed.Path = "/ai_sdlc_absent_connection_probe_574"
	query := parsed.Query()
	query.Del("dbname")
	query.Del("database")
	parsed.RawQuery = query.Encode()
	db, err := Open(context.Background(), parsed.String())
	if db != nil {
		db.Close()
		t.Fatal("unexpected database accepted")
	}
	if BootstrapConnectionBoundary(err) != "connection-database" {
		t.Fatal("real missing database was not classified")
	}
}
