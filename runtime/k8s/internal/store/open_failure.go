package store

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// Classes of Open failures. They are a fixed vocabulary: callers log or
// expose the class, never the driver error, which can carry the connection
// string.
const (
	// OpenInvalidConfiguration: the database URL cannot be parsed. Retrying
	// cannot help.
	OpenInvalidConfiguration = "invalid-configuration"
	// OpenRefused: the server answered and refused the login (bad password,
	// unknown role or missing database). PostgreSQL reports an unknown role
	// as a failed password, so a role the migrator has not provisioned yet
	// looks exactly like a wrong credential.
	OpenRefused = "refused"
	// OpenUnavailable: anything else, such as a refused or timed-out
	// connection or a server still starting.
	OpenUnavailable = "unavailable"
)

// OpenFailure classifies an error returned by Open.
func OpenFailure(err error) string {
	var parse *pgconn.ParseConfigError
	if errors.As(err, &parse) {
		return OpenInvalidConfiguration
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "28P01", "28000", "3D000":
			return OpenRefused
		}
	}
	return OpenUnavailable
}
