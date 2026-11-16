package store

import (
	"context"
	"errors"
	"net"
	"syscall"

	"github.com/jackc/pgx/v5/pgconn"
)

// BootstrapConnectionBoundary classifies an administrative connection failure
// using types/codes only. Driver messages, connection strings and SQL details
// must never enter the public installation diagnostic.
func BootstrapConnectionBoundary(err error) string {
	var parse *pgconn.ParseConfigError
	if errors.As(err, &parse) {
		return "connection-configuration"
	}
	var server *pgconn.PgError
	if errors.As(err, &server) {
		switch server.Code {
		case "28P01", "28000":
			return "connection-authentication"
		case "3D000":
			return "connection-database"
		case "42501":
			return "connection-permission"
		case "53300":
			return "connection-capacity"
		case "57P03":
			return "connection-unavailable"
		}
	}
	var network net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &network) && network.Timeout() {
		return "connection-timeout"
	}
	if errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM) {
		return "connection-permission"
	}
	var operation *net.OpError
	if errors.As(err, &operation) {
		return "connection-network"
	}
	return "connection"
}
