package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// AbortBeforeSend records only the transport's positively known no-send path.
// Its nonce identifies this invocation, so a competing failed BeforeCreate
// cannot close the successful invocation's send. The immutable outcome primary
// key makes a Pod ACK and not-sent mutually exclusive, including concurrent calls.
// This consumes the attempt; it neither replays CREATE nor produces health.
func (g BrowserGate) AbortBeforeSend(ctx context.Context, podDigest, attempt string) error {
	if g.Journal == nil || g.Journal.DB == nil || !workspacebundle.ValidDigest(podDigest) || !workspacebundle.ValidDigest(attempt) {
		return ErrExecution
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return g.Journal.WithTx(ctx, func(tx *sql.Tx) error {
		if AssertComponent(ctx, tx, "deployer") != nil {
			return ErrExecution
		}
		var until time.Time
		if tx.QueryRowContext(ctx, `SELECT expires_at FROM deployment_requests WHERE id=$1 AND claim_token=$2 AND state='claimed' FOR UPDATE`, g.Claim.ID, g.Claim.Token).Scan(&until) != nil || !until.Equal(g.Claim.Until) || !time.Now().Before(until) {
			return ErrExecution
		}
		created, e := g.readIntent(ctx, tx, podDigest)
		if e != nil {
			return e
		}
		var original string
		if tx.QueryRowContext(ctx, `SELECT attempt_nonce FROM deployment_browser_intents WHERE id=$1`, g.Request.RequestID).Scan(&original) != nil || original != attempt {
			return ErrExecution
		}
		cfg, _, e := deploymentArchived(ctx, tx, g.Claim)
		if e != nil || g.Journal.browserCleanupIdentity(ctx, BrowserCleanup{Profile: cfg.Browser, Target: cfg.Target}) != nil {
			return ErrExecution
		}
		var sent time.Time
		e = tx.QueryRowContext(ctx, `SELECT created_at FROM deployment_browser_sends WHERE id=$1 AND claim_token=$2`, g.Request.RequestID, g.Claim.Token).Scan(&sent)
		if e == sql.ErrNoRows {
			return nil
		}
		if e != nil || sent.Before(created) || sent.UnixMilli() >= g.Request.ExpiresAt {
			return ErrExecution
		}
		var recorded time.Time
		if tx.QueryRowContext(ctx, `INSERT INTO deployment_browser_pods(id,not_sent)VALUES($1,true) RETURNING recorded_at`, g.Request.RequestID).Scan(&recorded) != nil || recorded.Before(sent) || !recorded.Before(until) {
			return ErrExecution
		}
		if g.Journal.browserCleanupIdentity(ctx, BrowserCleanup{Profile: cfg.Browser, Target: cfg.Target}) != nil || ctx.Err() != nil || !time.Now().Before(until) {
			return ErrExecution
		}
		return nil
	})
}
