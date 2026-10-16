package planning

// reconcileRequest is the body for the stateless, dependency-free
// /v1/plans/reconcile dry-run endpoint (registered in events.go's Routes,
// alongside the two event-ingress endpoints ADR-0005 added).
type reconcileRequest struct {
	Plan          Plan `json:"plan"`
	MaxConcurrent int  `json:"maxConcurrent"`
}
