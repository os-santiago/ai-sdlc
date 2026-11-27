// Command flow-console serves the read-only AI-SDLC flow timeline.
//
// Journal mode (DATABASE_URL set) polls the authoritative PostgreSQL journal
// as the dedicated aisdlc_flow_console role and refuses any other identity.
// Fixture mode (-fixture or AI_SDLC_FLOW_CONSOLE_FIXTURE) replays recorded
// ai-sdlc.flow-event/v1 JSON lines without any database, for development and
// screenshots. See docs/operations/flow-console.md.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/flowconsole"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func duration(name, fallback string) time.Duration {
	d, err := time.ParseDuration(env(name, fallback))
	if err != nil {
		log.Fatalf("invalid %s", name)
	}
	return d
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	listen := flag.String("listen", env("AI_SDLC_HTTP_ADDRESS", ":8091"), "HTTP listen address")
	fixture := flag.String("fixture", os.Getenv("AI_SDLC_FLOW_CONSOLE_FIXTURE"), "replay this ai-sdlc.flow-event/v1 JSON lines file instead of the journal")
	live := flag.Duration("fixture-live", duration("AI_SDLC_FLOW_CONSOLE_FIXTURE_LIVE", "0s"), "shift the fixture to end this long after start and release it live")
	poll := flag.Duration("poll", duration("AI_SDLC_FLOW_CONSOLE_POLL", "3s"), "journal poll interval")
	window := flag.Duration("window", duration("AI_SDLC_FLOW_CONSOLE_WINDOW", "24h"), "journal window: work started within this duration")
	stale := flag.Duration("stale-wait", duration("AI_SDLC_FLOW_CONSOLE_STALE_WAIT", "10m"), "queued time after which a wait is stale")
	limit := flag.Int("limit", 5000, "rows per journal table per poll")
	capacity := flag.Int("capacity", 50000, "retained events")
	replay := flag.Int("replay", 5000, "events replayed to a new browser before live updates")
	synthetic := flag.Int("fixture-synthetic", 0, "instead of a fixture file, generate this many synthetic work items over the last day (UI load testing)")
	flag.Parse()
	if v := os.Getenv("AI_SDLC_FLOW_CONSOLE_LIMIT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return errors.New("invalid AI_SDLC_FLOW_CONSOLE_LIMIT")
		}
		*limit = n
	}
	if *poll < time.Second || *poll > time.Minute || *window < time.Minute || *window > 7*24*time.Hour || *stale < time.Minute || *limit < 1 || *limit > 20000 || *capacity < 100 || *capacity > 1000000 || *replay < 1 || *replay > 50000 {
		return errors.New("flow console limits out of range")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if *synthetic < 0 || *synthetic > 5000 {
		return errors.New("flow console limits out of range")
	}
	sources := 0
	for _, set := range []bool{*fixture != "", databaseURL != "", *synthetic > 0} {
		if set {
			sources++
		}
	}
	if sources != 1 {
		return errors.New("configure exactly one source: DATABASE_URL (journal), -fixture (recorded events) or -fixture-synthetic (generated events)")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var ready atomic.Bool
	var hub *flowconsole.Hub
	fatal := make(chan error, 1)
	if *synthetic > 0 {
		events := flowconsole.Synthetic(*synthetic, time.Now())
		hub = flowconsole.NewHub(*capacity, "fixture")
		go flowconsole.Fixture{Hub: hub, Events: events}.Run(ctx, time.Now())
		ready.Store(true)
		log.Printf("flow console serving %d synthetic events for %d work items", len(events), *synthetic)
	} else if *fixture != "" {
		file, err := os.Open(*fixture)
		if err != nil {
			return errors.New("fixture unavailable")
		}
		events, err := flowconsole.LoadFixture(file)
		file.Close()
		if err != nil {
			return err
		}
		hub = flowconsole.NewHub(*capacity, "fixture")
		go flowconsole.Fixture{Hub: hub, Events: events, Live: *live}.Run(ctx, time.Now())
		ready.Store(true)
		log.Printf("flow console replaying %d fixture events", len(events))
	} else {
		// Journal mode serves at once: liveness answers while the database is
		// still unreachable (a fresh install starts the console before
		// PostgreSQL and its role are ready), readiness stays false and the
		// page says it is waiting. Only a wrong database identity is fatal.
		hub = flowconsole.NewHub(*capacity, "journal")
		hub.SetStatus(flowconsole.Status{Source: "journal", Problem: flowconsole.ProblemConnecting, Window: window.String()})
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		go func() {
			db, err := connectJournal(ctx, func(ctx context.Context) (*store.DB, error) { return store.Open(ctx, databaseURL) },
				func(ctx context.Context, db *store.DB) error { return store.AssertFlowConsole(ctx, db.Pool) },
				hub, *window, journalBackoff, log.Print)
			if err != nil {
				if ctx.Err() == nil {
					fatal <- err
					cancel()
				}
				return
			}
			defer db.Close()
			collector := flowconsole.Collector{Hub: hub, Interval: *poll, Window: *window, Limit: *limit, Options: flowconsole.DeriveOptions{StaleWait: *stale},
				Read: func(ctx context.Context, since time.Time, limit int) (store.FlowJournal, error) {
					return store.ReadFlowJournal(ctx, db, since, limit)
				}}
			pollJournal(ctx, func(ctx context.Context) error {
				_, err := collector.Poll(ctx, time.Now())
				return err
			}, *poll, &ready, log.Print)
		}()
	}
	server := &http.Server{
		Addr:              *listen,
		Handler:           (&flowconsole.Server{Hub: hub, Ready: ready.Load, Replay: *replay}).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	log.Printf("flow console listening on %s", *listen)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return errors.New("flow console listener failed")
	}
	select {
	case err := <-fatal:
		return err
	default:
	}
	return nil
}

// backoff is a bounded retry schedule: Initial, doubling up to Max.
type backoff struct{ Initial, Max time.Duration }

// journalBackoff paces startup connection attempts: 1 s doubling to 30 s,
// so a database that becomes ready is noticed within 30 s.
var journalBackoff = backoff{Initial: time.Second, Max: 30 * time.Second}

func (b backoff) next(d time.Duration) time.Duration {
	if d < b.Initial {
		return b.Initial
	}
	if d *= 2; d > b.Max {
		return b.Max
	}
	return d
}

// connectJournal opens the journal and asserts the console's dedicated
// read-only role, retrying until ctx ends while the database is unreachable
// or the role check cannot be read (connection refused, the role not yet
// provisioned, PostgreSQL still starting), instead of exiting: on a fresh
// install that exit made the pod crash-loop until PostgreSQL was ready.
// Meanwhile the hub reports ProblemConnecting, so readiness and the page say
// the console is waiting. Any other identity is a configuration error and is
// returned at once, as before. Only transitions are logged, never the driver
// error.
func connectJournal(ctx context.Context, open func(context.Context) (*store.DB, error), assert func(context.Context, *store.DB) error, hub *flowconsole.Hub, window time.Duration, schedule backoff, logf func(...any)) (*store.DB, error) {
	var delay time.Duration
	for attempt := 1; ; attempt++ {
		db, err := open(ctx)
		if err == nil {
			check, cancel := context.WithTimeout(ctx, 5*time.Second)
			err = assert(check, db)
			cancel()
			if err == nil {
				if attempt > 1 {
					logf("flow journal database reachable after ", attempt, " attempts")
				}
				return db, nil
			}
			_ = db.Close()
			if !errors.Is(err, store.ErrObservationUnavailable) {
				return nil, errors.New("flow console requires its dedicated read-only database role")
			}
		}
		if attempt == 1 {
			logf("flow journal database unavailable; retrying every ", schedule.Initial, " to ", schedule.Max, " until it is reachable")
		}
		hub.SetStatus(flowconsole.Status{Source: "journal", Problem: flowconsole.ProblemConnecting, Window: window.String()})
		delay = schedule.next(delay)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// pollJournal reads the journal on every tick until ctx ends. Readiness
// latches after the first successful read: a later outage keeps serving the
// last known events (the page shows the source as unavailable) rather than
// removing the console from its Service. Only transitions are logged, never
// the driver error.
func pollJournal(ctx context.Context, poll func(context.Context) error, interval time.Duration, ready *atomic.Bool, logf func(...any)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	failing := false
	for {
		err := poll(ctx)
		if err == nil {
			ready.Store(true)
		}
		if (err != nil) != failing {
			failing = err != nil
			if failing {
				logf("flow journal read failed")
			} else {
				logf("flow journal read restored")
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
