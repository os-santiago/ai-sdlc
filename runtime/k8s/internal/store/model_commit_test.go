package store

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
)

// Drop the PostgreSQL COMMIT acknowledgement after the server has committed.
// This exercises a genuinely uncertain transport outcome, not a mock or a
// cancellation before the transaction. The proxy carries fixture credentials
// only and never logs protocol bytes.
func modelCommitFault(t *testing.T) (*DB, <-chan bool) {
	t.Helper()
	u, err := url.Parse(os.Getenv("AI_SDLC_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("sslmode") != "disable" {
		t.Skip("wire fault injection requires the isolated plaintext PostgreSQL fixture")
	}
	upstream := u.Host
	if u.Port() == "" {
		upstream = net.JoinHostPort(u.Hostname(), "5432")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	dropped := make(chan bool, 1)
	go func() {
		client, err := listener.Accept()
		if err != nil {
			dropped <- false
			return
		}
		defer client.Close()
		server, err := net.DialTimeout("tcp", upstream, 2*time.Second)
		if err != nil {
			dropped <- false
			return
		}
		defer server.Close()
		_ = client.SetDeadline(time.Now().Add(10 * time.Second))
		_ = server.SetDeadline(time.Now().Add(10 * time.Second))
		copied := make(chan struct{})
		go func() { _, _ = io.Copy(server, client); close(copied) }()
		defer func() { client.Close(); server.Close(); <-copied }()
		for {
			var header [5]byte
			if _, err = io.ReadFull(server, header[:]); err != nil {
				dropped <- false
				return
			}
			size := binary.BigEndian.Uint32(header[1:])
			if size < 4 || size > 1<<20 {
				dropped <- false
				return
			}
			body := make([]byte, int(size)-4)
			if _, err = io.ReadFull(server, body); err != nil {
				dropped <- false
				return
			}
			if header[0] == 'C' && string(body) == "COMMIT\x00" {
				dropped <- true
				return
			}
			if _, err = client.Write(header[:]); err != nil {
				dropped <- false
				return
			}
			if _, err = client.Write(body); err != nil {
				dropped <- false
				return
			}
		}
	}()
	u.Host = listener.Addr().String()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	faulted, err := Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { faulted.Close() })
	faulted.Pool.SetMaxOpenConns(1)
	return faulted, dropped
}

func TestModelLostCommitAcknowledgementNeverAuthorizesRetry(t *testing.T) {
	db, _, a := modelFixture(t, 2)
	ref := modelReserve(t, db, a, 1)
	faulted, dropped := modelCommitFault(t)
	ctx := context.Background()
	d, err := faulted.ClaimModelCall(ctx, ref)
	if err == nil || d.Invoke {
		t.Fatalf("uncertain commit exposed invocation: %+v %v", d, err)
	}
	if !<-dropped {
		t.Fatal("proxy did not drop the real commit acknowledgement")
	}
	var state string
	if err = db.Pool.QueryRowContext(ctx, `SELECT state FROM model_calls WHERE allocation_id=$1 AND sequence=$2`, a.ID, ref.Sequence).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "started" {
		t.Fatalf("server state=%s, expected committed started", state)
	}
	d, err = db.ClaimModelCall(ctx, ref)
	if err != nil || d.Invoke || d.Reason != "already-claimed" {
		t.Fatalf("restarted claim: %+v %v", d, err)
	}
	d, err = db.StopModelCall(ctx, ref)
	if err != nil || d.Reason != "ambiguous" {
		t.Fatalf("reconciliation: %+v %v", d, err)
	}
}

func TestModelLostCompletionCommitAcknowledgementBlocksNewCall(t *testing.T) {
	db, _, a := modelFixture(t, 3)
	ctx := context.Background()
	ref := modelReserve(t, db, a, 1)
	if d, err := db.ClaimModelCall(ctx, ref); err != nil || !d.Invoke {
		t.Fatalf("claim %+v %v", d, err)
	}
	faulted, dropped := modelCommitFault(t)
	d, err := faulted.FinishModelCall(ctx, ref, 200, handoffBody)
	if err == nil || d.Invoke || d.Call.State != "" {
		t.Fatalf("uncertain completion returned authority: %+v %v", d, err)
	}
	if !<-dropped {
		t.Fatal("completion COMMIT acknowledgement was not dropped")
	}
	var state string
	var handed bool
	if err := db.Pool.QueryRow(`SELECT state,response_handed_off_at IS NOT NULL FROM model_calls WHERE allocation_id=$1`, a.ID).Scan(&state, &handed); err != nil {
		t.Fatal(err)
	}
	if state != "completed" || handed {
		t.Fatal("server did not commit undelivered completion")
	}
	if d, err := db.ReserveModelCall(ctx, a.ID, a.PodUID, 2, modelTestPayload); err != nil || d.Reason != "response-handoff-pending" {
		t.Fatalf("uncertain completion admitted next call: %+v %v", d, err)
	}
	if d, err := db.ClaimModelCall(ctx, ref); err != nil || d.Invoke {
		t.Fatalf("uncertain completion permitted replay: %+v %v", d, err)
	}
	if d, err := db.StopModelCall(ctx, ref); err != nil || d.Reason != "ambiguous" {
		t.Fatalf("abandon failed: %+v %v", d, err)
	}
}
func TestModelLostHandoffCommitAcknowledgementReconcilesOnlyMetadata(t *testing.T) {
	db, _, a := modelFixture(t, 3)
	ctx := context.Background()
	ref := modelReserve(t, db, a, 1)
	modelComplete(t, db, ref)
	faulted, dropped := modelCommitFault(t)
	d, err := faulted.AcknowledgeModelResponse(ctx, ref, modelmailbox.Digest(handoffBody), 200, len(handoffBody))
	if err == nil || d.Invoke || d.Call.ResponseHandedOff {
		t.Fatalf("uncertain handoff returned authority: %+v %v", d, err)
	}
	if !<-dropped {
		t.Fatal("handoff COMMIT acknowledgement was not dropped")
	}
	var before, after time.Time
	if err := db.Pool.QueryRow(`SELECT response_handed_off_at FROM model_calls WHERE allocation_id=$1`, a.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if d := modelAck(t, db, ref); d.Reason != "existing" || !d.Call.ResponseHandedOff || d.Invoke {
		t.Fatal(d)
	}
	if err := db.Pool.QueryRow(`SELECT response_handed_off_at FROM model_calls WHERE allocation_id=$1`, a.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !before.Equal(after) {
		t.Fatal("reconciled handoff timestamp changed")
	}
	if d, err := db.ClaimModelCall(ctx, ref); err != nil || d.Invoke {
		t.Fatalf("ack replay enabled provider: %+v %v", d, err)
	}
	if d, err := db.ReserveModelCall(ctx, a.ID, a.PodUID, 2, modelTestPayload); err != nil || d.Reason != "reserved" {
		t.Fatalf("confirmed handoff blocked next reservation: %+v %v", d, err)
	}
}
