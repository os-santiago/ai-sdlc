package execution

import (
	"context"
	"errors"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasecompletion"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/terminalflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/terminalwriter"
	"testing"
	"time"
)

// Real PostgreSQL with synthetic GitHub replies; no production release proof.
type terminalTransportFixture struct {
	f                terminalFixture
	t                *testing.T
	calls            []string
	lost             string
	closeObservation bool
}

func (w *terminalTransportFixture) Execute(ctx context.Context, p releasecompletion.Plan, step string, id int64, fence terminalwriter.Fence) (releasecompletion.EffectReceipt, error) {
	deadline, ok := ctx.Deadline()
	if !ok || deadline.After(w.f.c.Until) {
		w.t.Fatal("unbounded transport")
	}
	q, e := releasecompletion.Request(p, step, id, time.Now())
	if e != nil {
		return releasecompletion.EffectReceipt{}, e
	}
	if e = fence(ctx, q); e != nil {
		return releasecompletion.EffectReceipt{}, e
	}
	w.calls = append(w.calls, step)
	if step == w.lost {
		return releasecompletion.EffectReceipt{}, errors.New("synthetic lost API ACK")
	}
	if step == "close" && w.closeObservation {
		if _, e := w.f.f.p.b.f.Admin.Pool.Exec(`UPDATE intake_heads SET generation=generation+1`); e != nil {
			w.t.Fatal(e)
		}
	}
	return w.f.receipt(q), nil
}
func TestTerminalControllerRecoversOnlyOriginalACKs(t *testing.T) {
	for _, stage := range []string{"claimed", "intent", "tag-sent", "tag-ack", "draft-ack", "publish-ack", "close-ack", "publish-lost", "close-lost"} {
		t.Run(stage, func(t *testing.T) {
			f := newTerminalFixture(t)
			ctx := context.Background()
			expected := 4
			switch stage {
			case "intent":
				if _, e := f.j.BeginTerminalEffect(ctx, f.c, "tag"); e != nil {
					t.Fatal(e)
				}
			case "tag-sent":
				f.send(t, "tag")
				expected = 0
			case "tag-ack":
				f.effect(t, "tag")
				expected = 3
			case "draft-ack":
				f.effect(t, "tag")
				f.effect(t, "draft")
				expected = 2
			case "publish-ack":
				for _, step := range []string{"tag", "draft", "publish"} {
					f.effect(t, step)
				}
				expected = 1
			case "close-ack":
				for _, step := range []string{"tag", "draft", "publish", "close"} {
					f.effect(t, step)
				}
				expected = 0
			case "publish-lost":
				expected = 3
			case "close-lost":
				expected = 4
			}
			transport := &terminalTransportFixture{f: f, t: t, closeObservation: true}
			if stage == "publish-lost" {
				transport.lost = "publish"
			}
			if stage == "close-lost" {
				transport.lost = "close"
			}
			controller := &terminalflow.Controller{Journal: f.j, Writer: transport}
			worked, e := controller.RunOne(ctx)
			failure := stage == "tag-sent" || stage == "publish-lost" || stage == "close-lost"
			if !worked || (e != nil) != failure || len(transport.calls) != expected {
				t.Fatal("recovery outcome", stage, worked, e, transport.calls)
			}
			var state, token string
			var until time.Time
			if e := f.j.Pool.QueryRow(`SELECT state,claim_token,expires_at FROM terminal_requests`).Scan(&state, &token, &until); e != nil {
				t.Fatal(e)
			}
			want := "completed"
			if failure {
				want = "quarantined"
			}
			if state != want || token != f.c.Token || !until.Equal(f.c.Until) {
				t.Fatal("authority renewed/outcome lost", state)
			}
			if worked, e = controller.RunOne(ctx); e != nil || worked || len(transport.calls) != expected {
				t.Fatal("terminal replay", worked, e)
			}
		})
	}
}
