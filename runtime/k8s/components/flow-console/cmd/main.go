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
	flag.Parse()
	if v := os.Getenv("AI_SDLC_FLOW_CONSOLE_LIMIT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return errors.New("invalid AI_SDLC_FLOW_CONSOLE_LIMIT")
		}
		*limit = n
	}
	if *poll < time.Second || *poll > time.Minute || *window < time.Minute || *window > 7*24*time.Hour || *stale < time.Minute || *limit < 1 || *limit > 20000 || *capacity < 100 || *capacity > 1000000 {
		return errors.New("flow console limits out of range")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if (*fixture == "") == (databaseURL == "") {
		return errors.New("configure exactly one source: DATABASE_URL (journal) or -fixture (recorded events)")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var ready atomic.Bool
	var hub *flowconsole.Hub
	if *fixture != "" {
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
		db, err := store.Open(ctx, databaseURL)
		if err != nil {
			return errors.New("journal database unavailable")
		}
		defer db.Close()
		check, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = store.AssertFlowConsole(check, db.Pool)
		cancel()
		if err != nil {
			return errors.New("flow console requires its dedicated read-only database role")
		}
		hub = flowconsole.NewHub(*capacity, "journal")
		collector := flowconsole.Collector{Hub: hub, Interval: *poll, Window: *window, Limit: *limit, Options: flowconsole.DeriveOptions{StaleWait: *stale},
			Read: func(ctx context.Context, since time.Time, limit int) (store.FlowJournal, error) {
				return store.ReadFlowJournal(ctx, db, since, limit)
			}}
		go pollJournal(ctx, func(ctx context.Context) error {
			_, err := collector.Poll(ctx, time.Now())
			return err
		}, *poll, &ready, log.Print)
	}
	server := &http.Server{
		Addr:              *listen,
		Handler:           (&flowconsole.Server{Hub: hub, Ready: ready.Load}).Handler(),
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
	return nil
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
