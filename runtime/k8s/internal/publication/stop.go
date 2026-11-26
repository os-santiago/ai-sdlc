package publication

import (
	"errors"
	"strconv"
)

// transportStop is a writer failure with a bounded class for the stop log. It
// is still ErrPublication: every class leaves the effect uncertain exactly as
// before. It never carries a response body, URL, credential or candidate text.
type transportStop struct{ class string }

func (e transportStop) Error() string { return ErrPublication.Error() }
func (e transportStop) Unwrap() error { return ErrPublication }

// Classes of a writer stop. "transport" means the request may or may not have
// reached the forge (connection, TLS or deadline); a "status-" class means the
// forge answered with that HTTP status; "response" means a 201 whose body or
// headers were refused; "mismatch" means a decoded 201 that is not the planned
// object; "fence" means current authority was lost after the response.
const (
	StopTransport = "transport"
	StopResponse  = "response"
	StopMismatch  = "mismatch"
	StopFence     = "fence"
)

func statusClass(code int) string {
	switch code {
	case 200, 202, 204, 301, 302, 304, 307, 308, 400, 401, 403, 404, 409, 410, 413, 415, 422, 429, 500, 502, 503, 504:
		return "status-" + strconv.Itoa(code)
	}
	if code >= 100 && code < 600 {
		return "status-" + strconv.Itoa(code/100) + "xx"
	}
	return "status-invalid"
}

// StopClass names the bounded class of a writer failure, or "" when the error
// carries none (for example a plan refused before any network call).
func StopClass(err error) string {
	var stop transportStop
	if errors.As(err, &stop) {
		return stop.class
	}
	return ""
}
