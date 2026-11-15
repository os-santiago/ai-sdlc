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
)

// Drop the PostgreSQL COMMIT acknowledgement after the server has committed.
// This exercises a genuinely uncertain transport outcome, not a mock or a
// cancellation before the transaction. The proxy carries fixture credentials
// only and never logs protocol bytes.
func TestModelLostCommitAcknowledgementNeverAuthorizesRetry(t *testing.T) {
	db, _, a := modelFixture(t, 2)
	ref := modelReserve(t, db, a, 1)
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
	defer listener.Close()
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
	defer cancel()
	faulted, err := Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer faulted.Close()
	faulted.Pool.SetMaxOpenConns(1)
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
