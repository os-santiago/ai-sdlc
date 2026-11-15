package modelmailbox

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store, dir
}

func TestRejectsHostileFileTypesWithoutBlocking(t *testing.T) {
	for _, kind := range []string{"symlink", "fifo", "directory", "hardlink", "oversize"} {
		t.Run(kind, func(t *testing.T) {
			s, dir := newTestStore(t)
			target := filepath.Join(dir, requestFile)
			outside := filepath.Join(t.TempDir(), "outside")
			if err := os.WriteFile(outside, []byte("sentinel"), 0600); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(outside, target)
			case "fifo":
				err = syscall.Mkfifo(target, 0600)
			case "directory":
				err = os.Mkdir(target, 0700)
			case "hardlink":
				err = os.Link(outside, target)
			case "oversize":
				err = os.WriteFile(target, bytes.Repeat([]byte("x"), MaxEnvelopeBytes+1), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { _, err := s.ReadRequest(testIdentity); done <- err }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("accepted unsafe file")
				}
			case <-time.After(time.Second):
				t.Fatal("blocked on unsafe file")
			}
			got, _ := os.ReadFile(outside)
			if string(got) != "sentinel" {
				t.Fatal("modified external file")
			}
		})
	}
}

func TestDirectoryDescriptorAndFlatNames(t *testing.T) {
	s, dir := newTestStore(t)
	e := testRequest()
	if err := s.publish(requestFile, e); err != nil {
		t.Fatal(err)
	}
	renamed := dir + "-renamed"
	if err := os.Rename(dir, renamed); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(renamed) })
	if err := os.Symlink(t.TempDir(), dir); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(dir); err == nil {
		t.Fatal("followed root symlink")
	}
	got, err := s.ReadRequest(testIdentity)
	if err != nil || got.RequestSHA256 != e.RequestSHA256 {
		t.Fatal("lost opened directory identity", err)
	}
	for _, name := range []string{"../request.json", "nested/request.json", "/etc/passwd", ".publish-x"} {
		if _, err := s.read(name); err == nil {
			t.Fatal("accepted pathname", name)
		}
	}
}

func TestResponseCorrelationAndLateWrites(t *testing.T) {
	s, dir := newTestStore(t)
	request := testRequest()
	response := testResponse(request)
	if err := s.WriteResponse(testIdentity, response); err == nil {
		t.Fatal("accepted response without pending request")
	}
	if err := s.publish(requestFile, request); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Envelope){func(e *Envelope) { e.Sequence++ }, func(e *Envelope) { e.ExpiresAt++ }, func(e *Envelope) { e.PodUID = "00000000-0000-0000-0000-000000000000" }} {
		forged := response
		change(&forged)
		if s.WriteResponse(testIdentity, forged) == nil {
			t.Fatal("accepted mismatched response")
		}
	}
	if err := s.WriteResponse(testIdentity, response); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteResponse(testIdentity, response); err == nil {
		t.Fatal("overwrote published response")
	}
	if err := s.remove(requestFile); err != nil {
		t.Fatal(err)
	}
	s.remove(responseFile)
	if s.WriteResponse(testIdentity, response) == nil {
		t.Fatal("accepted late response")
	}
	outside := filepath.Join(t.TempDir(), "outside")
	os.WriteFile(outside, []byte("sentinel"), 0600)
	if err := s.publish(requestFile, request); err != nil {
		t.Fatal(err)
	}
	os.Symlink(outside, filepath.Join(dir, responseFile))
	if s.WriteResponse(testIdentity, response) == nil {
		t.Fatal("accepted symlink destination")
	}
	got, _ := os.ReadFile(outside)
	if string(got) != "sentinel" {
		t.Fatal("followed response symlink")
	}
}

func TestGrowingEnvelopeRemainsBounded(t *testing.T) {
	s, dir := newTestStore(t)
	path := filepath.Join(dir, requestFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer f.Close()
		block := bytes.Repeat([]byte("x"), 1024)
		for i := 0; i < 2*MaxEnvelopeBytes/1024; i++ {
			if _, err := f.Write(block); err != nil {
				return
			}
		}
	}()
	for i := 0; i < 10; i++ {
		raw, err := s.read(requestFile)
		if err == nil && len(raw) > MaxEnvelopeBytes {
			t.Fatal("unbounded read")
		}
	}
	<-done
	if _, err := s.read(requestFile); err == nil {
		t.Fatal("accepted oversized grown file")
	}
}

func TestConcurrentResponsesNeverReplaceWinner(t *testing.T) {
	s, _ := newTestStore(t)
	request := testRequest()
	if err := s.publish(requestFile, request); err != nil {
		t.Fatal(err)
	}
	for iteration := 0; iteration < 50; iteration++ {
		start := make(chan struct{})
		type outcome struct {
			payload []byte
			err     error
		}
		results := make(chan outcome, 2)
		for _, payload := range []string{`{"winner":"a"}`, `{"winner":"b"}`} {
			go func(payload string) {
				response := testResponse(request)
				response.Payload = []byte(payload)
				<-start
				results <- outcome{response.Payload, s.WriteResponse(testIdentity, response)}
			}(payload)
		}
		close(start)
		var winner []byte
		for i := 0; i < 2; i++ {
			result := <-results
			if result.err == nil {
				if winner != nil {
					t.Fatal("two responses published")
				}
				winner = result.payload
			}
		}
		if winner == nil {
			t.Fatal("neither valid response published")
		}
		raw, err := s.read(responseFile)
		if err != nil {
			t.Fatal(err)
		}
		actual, err := Decode(raw)
		if err != nil || !bytes.Equal(actual.Payload, winner) {
			t.Fatal("winner was replaced", err)
		}
		if err := s.remove(responseFile); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNewServerRejectsStalePendingState(t *testing.T) {
	s, _ := newTestStore(t)
	e := testRequest()
	if err := s.publish(requestFile, e); err != nil {
		t.Fatal(err)
	}
	if _, err := NewServer(s, testIdentity, time.Second); err == nil {
		t.Fatal("reused pending state")
	}
	s.remove(requestFile)
	if _, err := s.ReadRequest(testIdentity); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(e)
	decoded, err := Decode(raw)
	if err != nil || decoded.Sequence != 1 {
		t.Fatal(err)
	}
}
