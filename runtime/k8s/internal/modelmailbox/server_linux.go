package modelmailbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"sync"
	"time"
)

type Server struct {
	store    *Store
	id       Identity
	wait     time.Duration
	active   chan struct{}
	sequence uint64 // accessed only while holding active
}

func NewServer(store *Store, id Identity, wait time.Duration) (*Server, error) {
	if store == nil || id.Validate() != nil || wait < time.Millisecond || wait > 120*time.Second {
		return nil, errors.New("invalid mailbox server configuration")
	}
	for _, name := range []string{requestFile, responseFile} {
		if _, err := store.read(name); !errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("mailbox is not empty; use a new attempt volume")
		}
	}
	return &Server{store: store, id: id, wait: wait, active: make(chan struct{}, 1)}, nil
}

func failure(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": "mailbox", "message": code}})
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/healthz" && r.URL.RawQuery == "" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"mailbox-only"}`))
		return
	}
	if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" || r.URL.RawQuery != "" {
		failure(w, 404, "unsupported_route")
		return
	}
	select {
	case s.active <- struct{}{}:
		defer func() { <-s.active }()
	default:
		failure(w, 429, "request_already_pending")
		return
	}
	if s.sequence >= MaxRequests {
		failure(w, 429, "local_request_limit")
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		failure(w, 415, "json_required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		failure(w, 413, "request_too_large")
		return
	}
	if ValidateRequest(body) != nil {
		failure(w, 400, "invalid_chat_request")
		return
	}
	s.sequence++
	request := Envelope{Version: Version, Kind: "request", Attempt: s.id.Attempt, PodUID: s.id.PodUID, Sequence: s.sequence, RequestSHA256: Digest(body), Payload: body, ExpiresAt: time.Now().Add(s.wait).UnixMilli()}
	if err := s.store.publish(requestFile, request); err != nil {
		log.Print(err) // fixed operation and errno only; never payloads
		failure(w, 502, "mailbox_unavailable")
		return
	}
	defer s.store.remove(requestFile)
	defer s.store.remove(responseFile)
	ctx, cancel := context.WithTimeout(r.Context(), s.wait)
	defer cancel()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			failure(w, 504, "mailbox_deadline")
			return
		case <-tick.C:
			raw, err := s.store.read(responseFile)
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrPending) {
				continue
			}
			if err != nil {
				failure(w, 502, "unsafe_response")
				return
			}
			response, err := Decode(raw)
			if err != nil || response.Validate(s.id, "response") != nil || response.Sequence != request.Sequence || response.RequestSHA256 != request.RequestSHA256 || response.ExpiresAt != request.ExpiresAt {
				failure(w, 502, "response_mismatch")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(response.Status)
			_, _ = w.Write(response.Payload)
			return
		}
	}
}

// Listen binds exclusively to IPv4 loopback. No user-supplied bind address,
// upstream URL, headers or credentials are accepted by this component.
func (s *Server) Listen(ctx context.Context) error {
	base, err := net.Listen("tcp4", "127.0.0.1:8091")
	if err != nil {
		return errors.New("cannot bind mailbox loopback listener")
	}
	listener := &boundedListener{Listener: base, slots: make(chan struct{}, 8), done: make(chan struct{})}
	server := &http.Server{Handler: s, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: s.wait + 5*time.Second, IdleTimeout: 10 * time.Second, MaxHeaderBytes: 8192}
	go func() { <-ctx.Done(); _ = server.Close() }()
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

type boundedListener struct {
	net.Listener
	slots chan struct{}
	done  chan struct{}
	once  sync.Once
}

func (l *boundedListener) Accept() (net.Conn, error) {
	select {
	case l.slots <- struct{}{}:
	case <-l.done:
		return nil, net.ErrClosed
	}
	conn, err := l.Listener.Accept()
	if err != nil {
		<-l.slots
		return nil, err
	}
	return &boundedConn{Conn: conn, release: func() { <-l.slots }}, nil
}
func (l *boundedListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return l.Listener.Close()
}

type boundedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *boundedConn) Close() error { err := c.Conn.Close(); c.once.Do(c.release); return err }
