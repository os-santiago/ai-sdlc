package store

import (
	"context"
	"net/url"
	"os"
	"testing"
)

// Each component samples only its own queues, with privileges it already has,
// and row security keeps the outbox count to its own messages.
func TestQueueDepthReadsOnlyOwnQueuesUnderComponentRoles(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	passwords := mapRolePasswords()
	if err := db.ProvisionRolesForCapabilities(ctx, passwords, "verification", true, true); err != nil {
		t.Fatal(err)
	}
	roles := map[string]*DB{}
	for _, component := range []string{"orchestrator", "worker", "verifier", "release-manager", "relay"} {
		u, err := url.Parse(os.Getenv("AI_SDLC_TEST_DATABASE_URL"))
		if err != nil {
			t.Fatal(err)
		}
		role, _ := ComponentRole(component)
		u.User = url.UserPassword(role, passwords[component])
		connected, err := Open(ctx, u.String())
		if err != nil {
			t.Fatal("component connection failed")
		}
		t.Cleanup(func() { _ = connected.Close() })
		roles[component] = connected
	}
	before := map[string]int64{}
	for component, stages := range map[string][]string{
		"orchestrator": {"outbox"}, "worker": {"outbox", "execution"},
		"verifier": {"outbox", "verification", "review"}, "release-manager": {"outbox", "publication"},
	} {
		for _, stage := range stages {
			depth, err := roles[component].QueueDepth(ctx, component, stage)
			if err != nil || depth < 0 || depth > MaxQueueDepth {
				t.Fatal(component, stage, depth, err)
			}
			before[component+"/"+stage] = depth
		}
	}
	event := NewID()
	// The component cannot delete outbox rows; the test owner removes this
	// one row, even when a later assertion fails.
	t.Cleanup(func() {
		_, _ = db.Pool.ExecContext(context.Background(), `DELETE FROM authorized_outbox WHERE producer='orchestrator' AND event_id=$1`, event)
	})
	if _, err := roles["orchestrator"].Pool.ExecContext(ctx, `INSERT INTO authorized_outbox(producer,event_id,target,operation,receipt,payload) VALUES('orchestrator',$1,'worker','execute-work','{}','\x01')`, event); err != nil {
		t.Fatal(err)
	}
	if depth, err := roles["orchestrator"].QueueDepth(ctx, "orchestrator", "outbox"); err != nil || depth != before["orchestrator/outbox"]+1 {
		t.Fatal("own outbox message not counted", depth, err)
	}
	if depth, err := roles["worker"].QueueDepth(ctx, "worker", "outbox"); err != nil || depth != before["worker/outbox"] {
		t.Fatal("another producer's message counted", depth, err)
	}
	for _, refused := range []struct{ db, component, stage string }{
		{"orchestrator", "orchestrator", "execution"}, // a queue it does not own
		{"worker", "worker", "review"},
		{"verifier", "verifier", "publication"},
		{"relay", "relay", "outbox"},           // the relay transports every outbox
		{"worker", "verifier", "verification"}, // a role claiming another identity
		{"worker", "worker", "planning; DROP TABLE plans"},
		{"worker", "unknown", "outbox"},
	} {
		if _, err := roles[refused.db].QueueDepth(ctx, refused.component, refused.stage); err != ErrQueueDepth {
			t.Fatal("queue depth not refused", refused, err)
		}
	}
	// The migration credential is not a component identity.
	if _, err := db.QueueDepth(ctx, "worker", "execution"); err != ErrQueueDepth {
		t.Fatal("privileged credential sampled a queue", err)
	}
}
