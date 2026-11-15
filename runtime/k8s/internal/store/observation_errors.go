package store

import (
	"database/sql"
	"errors"
)

// ErrObservationUnavailable is sanitized and grants no authority. A queued
// request may be observed again before expiry; an active effect is never replayed.
var ErrObservationUnavailable = errors.New("database observation unavailable")

func observationError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrExecution
	}
	return ErrObservationUnavailable
}
func identityObservationError(err error) error {
	if errors.Is(err, ErrObservationUnavailable) {
		return ErrObservationUnavailable
	}
	return ErrExecution
}
