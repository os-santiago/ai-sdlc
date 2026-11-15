package modelbroker

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

func TestBrokerAuthorityRequiredBeforeReservation(t *testing.T) {
	for _, which := range []string{"missing", "denied", "expired", "panic", "cancelled"} {
		t.Run(which, func(t *testing.T) {
			var invoked, published atomic.Int32
			d, a, profile, request := brokerFixture(t, func(w http.ResponseWriter, r *http.Request) {
				invoked.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.Write(completionBody)
			}, publisherFunc(func(context.Context, modelmailbox.Envelope) error { published.Add(1); return nil }))
			d.Authority = fixtureAuthority(func(ctx context.Context, scope store.ModelScope) (time.Time, error) {
				switch which {
				case "denied":
					return time.Time{}, errors.New("revoked")
				case "expired":
					return time.Now().Add(-time.Second), nil
				case "panic":
					panic("fixture authority")
				}
				<-ctx.Done()
				return scope.ExpiresAt, nil
			})
			if which == "missing" {
				d.Authority = nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			if result, err := d.Deliver(ctx, a.ID, profile, request); err == nil || result.DispatchAttempted || result.HandoffConfirmed {
				t.Fatal("invalid authority accepted")
			}
			var count int
			if err := d.DB.Pool.QueryRow(`SELECT count(*) FROM model_calls WHERE allocation_id=$1`, a.ID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 0 || invoked.Load() != 0 || published.Load() != 0 {
				t.Fatal("denied authority caused effects")
			}
		})
	}
}
func TestBrokerAuthorityRevokedWhileWaitingForClaim(t *testing.T) {
	var invoked, published atomic.Int32
	var revoked atomic.Bool
	d, a, profile, request := brokerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		invoked.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write(completionBody)
	}, publisherFunc(func(context.Context, modelmailbox.Envelope) error { published.Add(1); return nil }))
	first := make(chan struct{})
	var checks atomic.Int32
	d.Authority = fixtureAuthority(func(ctx context.Context, scope store.ModelScope) (time.Time, error) {
		if checks.Add(1) == 1 {
			close(first)
		}
		if revoked.Load() {
			return time.Time{}, errors.New("revoked")
		}
		return scope.ExpiresAt, ctx.Err()
	})
	lock, err := d.DB.Pool.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	if _, err = lock.Exec(`SELECT work_item_uuid FROM model_budgets WHERE work_item_uuid=$1 FOR UPDATE`, a.WorkItem); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := d.Deliver(context.Background(), a.ID, profile, request); done <- err }()
	<-first
	deadline := time.Now().Add(time.Second)
	blocked := false
	for time.Now().Before(deadline) {
		var count int
		if err = d.DB.Pool.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE wait_event_type='Lock' AND query LIKE '%model_budgets%FOR UPDATE%'`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count > 0 {
			blocked = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !blocked {
		t.Fatal("actual reservation lock wait not observed")
	}
	revoked.Store(true)
	if err = lock.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = <-done; !errors.Is(err, ErrUncertain) {
		t.Fatal("revocation after claim was not uncertain")
	}
	parent, _, handed, _ := ledgerState(t, d.DB, a)
	if parent != "stopped" || handed || invoked.Load() != 0 || published.Load() != 0 || checks.Load() != 2 {
		t.Fatal("revoked committed claim caused provider effects")
	}
}
func TestBrokerAuthorityRevokedDuringProviderDoesNotPublish(t *testing.T) {
	var revoked atomic.Bool
	var invoked, published atomic.Int32
	d, a, profile, request := brokerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		invoked.Add(1)
		revoked.Store(true)
		w.Header().Set("Content-Type", "application/json")
		w.Write(completionBody)
	}, publisherFunc(func(context.Context, modelmailbox.Envelope) error { published.Add(1); return nil }))
	d.Authority = fixtureAuthority(func(ctx context.Context, scope store.ModelScope) (time.Time, error) {
		if revoked.Load() {
			return time.Time{}, errors.New("revoked")
		}
		return scope.ExpiresAt, ctx.Err()
	})
	result, err := d.Deliver(context.Background(), a.ID, profile, request)
	if !errors.Is(err, ErrUncertain) || !result.DispatchAttempted || result.HandoffConfirmed {
		t.Fatal("revoked response accepted")
	}
	parent, state, handed, _ := ledgerState(t, d.DB, a)
	var hash string
	var outputBytes, status int
	if err = d.DB.Pool.QueryRow(`SELECT response_sha256,output_bytes,response_status FROM model_calls WHERE allocation_id=$1`, a.ID).Scan(&hash, &outputBytes, &status); err != nil || hash != modelmailbox.Digest(completionBody) || outputBytes != len(completionBody) || status != 200 {
		t.Fatal("known response metadata lost")
	}
	if parent != "stopped" || state != "ambiguous" || handed || invoked.Load() != 1 || published.Load() != 0 {
		t.Fatal("response evidence lost or published after revocation")
	}
}
func TestBrokerAuthorityDeadlineCannotBeExtendedByLaterCheck(t *testing.T) {
	var invoked, published, checks atomic.Int32
	d, a, profile, request := brokerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		invoked.Add(1)
		select {
		case <-r.Context().Done():
			return
		case <-time.After(time.Second):
			w.Write(completionBody)
		}
	}, publisherFunc(func(context.Context, modelmailbox.Envelope) error { published.Add(1); return nil }))
	d.Authority = fixtureAuthority(func(ctx context.Context, scope store.ModelScope) (time.Time, error) {
		if checks.Add(1) == 1 {
			return time.Now().Add(100 * time.Millisecond), nil
		}
		return scope.ExpiresAt, nil
	})
	started := time.Now()
	result, err := d.Deliver(context.Background(), a.ID, profile, request)
	if !errors.Is(err, ErrUncertain) || !result.DispatchAttempted || published.Load() != 0 || invoked.Load() != 1 || time.Since(started) > time.Second {
		t.Fatal("later authority check extended delivery")
	}
	parent, _, _, _ := ledgerState(t, d.DB, a)
	if parent != "stopped" {
		t.Fatal("expired authority did not stop parent")
	}
}
func TestBrokerDuplicateHandoffStillRequiresCurrentAuthority(t *testing.T) {
	var invoked, published atomic.Int32
	d, a, profile, request := brokerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		invoked.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write(completionBody)
	}, publisherFunc(func(context.Context, modelmailbox.Envelope) error { published.Add(1); return nil }))
	if _, err := d.Deliver(context.Background(), a.ID, profile, request); err != nil {
		t.Fatal(err)
	}
	d.Authority = fixtureAuthority(func(context.Context, store.ModelScope) (time.Time, error) { return time.Time{}, errors.New("revoked") })
	if _, err := d.Deliver(context.Background(), a.ID, profile, request); !errors.Is(err, ErrRejected) {
		t.Fatal("duplicate bypassed current authority")
	}
	if invoked.Load() != 1 || published.Load() != 1 {
		t.Fatal("duplicate replayed effect")
	}
}
