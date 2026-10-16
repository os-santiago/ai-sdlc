package cycle

// evaluateRequest is the body for the stateless, dependency-free
// /v1/cycles/evaluate dry-run endpoint (registered in events.go's Routes,
// alongside /v1/attempts/dispatch, which ADR-0005 added).
type evaluateRequest struct {
	Attempt Attempt `json:"attempt"`
	Policy  Policy  `json:"policy"`
}
