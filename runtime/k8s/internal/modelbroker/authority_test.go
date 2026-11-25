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
	if result.FailureStage != "authorization-publication-authority" {
		t.Fatal("unnamed revocation stage", result.FailureStage)
	}
}

type fixtureAuthorityStop string

func (e fixtureAuthorityStop) Error() string         { return "sensitive arbitrary exception" }
func (e fixtureAuthorityStop) AuthorityStep() string { return string(e) }

func TestBrokerNamesTheAuthorityRefusalStep(t *testing.T) {
	for step, want := range map[string]string{"model-policy": "authorization-publication-model-policy", "sensitive arbitrary exception": "authorization-publication-authority"} {
		t.Run(want, func(t *testing.T) {
			var revoked atomic.Bool
			d, a, profile, request := brokerFixture(t, func(w http.ResponseWriter, r *http.Request) {
				revoked.Store(true)
				w.Header().Set("Content-Type", "application/json")
				w.Write(completionBody)
			}, publisherFunc(func(context.Context, modelmailbox.Envelope) error { return nil }))
			d.Authority = fixtureAuthority(func(ctx context.Context, scope store.ModelScope) (time.Time, error) {
				if revoked.Load() {
					return time.Time{}, fixtureAuthorityStop(step)
				}
				return scope.ExpiresAt, ctx.Err()
			})
			result, err := d.Deliver(context.Background(), a.ID, profile, request)
			if !errors.Is(err, ErrUncertain) || result.HandoffConfirmed || result.FailureStage != want {
				t.Fatalf("stage %q, want %q", result.FailureStage, want)
			}
		})
	}
}

func TestAuthorizationRefusalStagesAreBounded(t *testing.T) {
	for _, good := range []string{"authorization-reservation-busy", "authorization-dispatch-claim-expired", "authorization-publication-snapshot", "authorization-publication-snapshot-stale", "authorization-publication-scope-expired"} {
		if SafeFailureStage(good) != good {
			t.Fatal(good)
		}
	}
	for _, bad := range []string{"authorization-completion-busy", "authorization-publication-", "authorization-publication-sensitive", "authorization-publication-busy-x"} {
		if SafeFailureStage(bad) != "unknown" {
			t.Fatal(bad)
		}
	}
	if refusedStage("authorization-dispatch", errors.New("other")) != "authorization-dispatch" || refusedStage("authorization-dispatch", authorizationRefusal("deadline")) != "authorization-dispatch-deadline" {
		t.Fatal("refused stage")
	}
	if !errors.Is(authorizationRefusal("busy"), ErrRejected) {
		t.Fatal("refusal must unwrap to ErrRejected")
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

// fixtureStaleEvidence is the execution lifecycle's snapshot-stale refusal:
// unchanged issue evidence older than its window, with a bounded wait.
type fixtureStaleEvidence time.Duration

func (e fixtureStaleEvidence) Error() string               { return "sensitive arbitrary exception" }
func (e fixtureStaleEvidence) AuthorityStep() string       { return "snapshot-stale" }
func (e fixtureStaleEvidence) EvidenceWait() time.Duration { return time.Duration(e) }

func fastStaleEvidenceRetry(t *testing.T) {
	t.Helper()
	previous := staleEvidenceRetry
	staleEvidenceRetry = 10 * time.Millisecond
	t.Cleanup(func() { staleEvidenceRetry = previous })
}

// After the provider answered, publication authorization refuses with the
// given error until fresh is reached (or forever when fresh is negative).
func publicationAuthorityFixture(t *testing.T, refusal error, fresh int32) (Result, int32, int32, time.Duration, error) {
	t.Helper()
	fastStaleEvidenceRetry(t)
	var answered atomic.Bool
	var published, refusals atomic.Int32
	d, a, profile, request := brokerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		answered.Store(true)
		w.Header().Set("Content-Type", "application/json")
		w.Write(completionBody)
	}, publisherFunc(func(context.Context, modelmailbox.Envelope) error { published.Add(1); return nil }))
	d.Authority = fixtureAuthority(func(ctx context.Context, scope store.ModelScope) (time.Time, error) {
		if answered.Load() && (fresh < 0 || refusals.Load() < fresh) {
			refusals.Add(1)
			return time.Time{}, refusal
		}
		return scope.ExpiresAt, ctx.Err()
	})
	started := time.Now()
	result, err := d.Deliver(context.Background(), a.ID, profile, request)
	return result, refusals.Load(), published.Load(), time.Since(started), err
}

func TestBrokerWaitsForStaleEvidenceToBecomeFresh(t *testing.T) {
	result, refusals, published, _, err := publicationAuthorityFixture(t, fixtureStaleEvidence(time.Second), 3)
	if err != nil || !result.HandoffConfirmed || result.Outcome != "handed-off" || published != 1 || refusals != 3 {
		t.Fatalf("fresh evidence within the bound did not proceed: err=%v stage=%q refusals=%d published=%d", err, result.FailureStage, refusals, published)
	}
}

func TestBrokerStaleEvidenceThatStaysStaleRefusesByName(t *testing.T) {
	result, refusals, published, took, err := publicationAuthorityFixture(t, fixtureStaleEvidence(200*time.Millisecond), -1)
	if !errors.Is(err, ErrUncertain) || result.HandoffConfirmed || published != 0 || result.FailureStage != "authorization-publication-snapshot-stale" {
		t.Fatalf("stale evidence accepted or unnamed: err=%v stage=%q published=%d", err, result.FailureStage, published)
	}
	if refusals < 2 || took < 150*time.Millisecond || took > 5*time.Second {
		t.Fatalf("wait not bounded by the refusal: refusals=%d took=%v", refusals, took)
	}
}

func TestBrokerChangedSnapshotRefusesWithoutWaiting(t *testing.T) {
	for step, refusal := range map[string]error{"snapshot": fixtureAuthorityStop("snapshot"), "authority": errors.New("revoked")} {
		t.Run(step, func(t *testing.T) {
			result, refusals, published, _, err := publicationAuthorityFixture(t, refusal, -1)
			if !errors.Is(err, ErrUncertain) || published != 0 || refusals != 1 || result.FailureStage != "authorization-publication-"+step {
				t.Fatalf("changed snapshot waited or proceeded: err=%v stage=%q refusals=%d published=%d", err, result.FailureStage, refusals, published)
			}
		})
	}
}

// A stale-evidence wait never outlasts the mailbox: the request expires in a
// minute, so even an hour-long bound refuses before then.
func TestBrokerStaleEvidenceWaitEndsBeforeTheMailboxDeadline(t *testing.T) {
	fastStaleEvidenceRetry(t)
	d, a, profile, request := brokerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(completionBody)
	}, publisherFunc(func(context.Context, modelmailbox.Envelope) error { return nil }))
	request.ExpiresAt = time.Now().Add(15*time.Second + 300*time.Millisecond).UnixMilli()
	var checks atomic.Int32
	d.Authority = fixtureAuthority(func(ctx context.Context, scope store.ModelScope) (time.Time, error) {
		checks.Add(1)
		return time.Time{}, fixtureStaleEvidence(time.Hour)
	})
	started := time.Now()
	result, err := d.Deliver(context.Background(), a.ID, profile, request)
	if err == nil || result.FailureStage != "authorization-reservation-snapshot-stale" || time.Since(started) > 2*time.Second || checks.Load() < 2 {
		t.Fatalf("wait outlasted the mailbox: err=%v stage=%q took=%v checks=%d", err, result.FailureStage, time.Since(started), checks.Load())
	}
}
