package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func validToolRecord() ToolCallRecord {
	return ToolCallRecord{PipelineRun: "maven-build-test-x7k2p", Revision: strings.Repeat("a", 40), Tree: strings.Repeat("b", 40), SummarySHA256: strings.Repeat("c", 64), State: "passed"}
}

func TestToolCallRecordValidation(t *testing.T) {
	if err := validToolRecord().Validate(); err != nil {
		t.Fatal("valid record refused", err)
	}
	for name, accepted := range map[string]ToolCallRecord{
		"failed without tree": {PipelineRun: "run", Revision: strings.Repeat("a", 40), SummarySHA256: strings.Repeat("c", 64), State: "failed"},
		"error before run":    {Revision: strings.Repeat("a", 40), State: "error"},
		"error empty":         {State: "error"},
	} {
		if err := accepted.Validate(); err != nil {
			t.Fatal(name, err)
		}
	}
	for name, mutate := range map[string]func(*ToolCallRecord){
		"started state":      func(r *ToolCallRecord) { r.State = "started" },
		"abandoned state":    func(r *ToolCallRecord) { r.State = "abandoned" },
		"unknown state":      func(r *ToolCallRecord) { r.State = "PASSED" },
		"passed without run": func(r *ToolCallRecord) { r.PipelineRun = "" },
		"failed without revision": func(r *ToolCallRecord) {
			r.State, r.Revision = "failed", ""
		},
		"passed without summary": func(r *ToolCallRecord) { r.SummarySHA256 = "" },
		"run uppercase":          func(r *ToolCallRecord) { r.PipelineRun = "Run" },
		"run trailing dash":      func(r *ToolCallRecord) { r.PipelineRun = "run-" },
		"run too long":           func(r *ToolCallRecord) { r.PipelineRun = strings.Repeat("a", 64) },
		"run with dot":           func(r *ToolCallRecord) { r.PipelineRun = "a.b" },
		"short revision":         func(r *ToolCallRecord) { r.Revision = strings.Repeat("a", 39) },
		"uppercase tree":         func(r *ToolCallRecord) { r.Tree = strings.Repeat("A", 40) },
		"bad summary":            func(r *ToolCallRecord) { r.SummarySHA256 = strings.Repeat("z", 64) },
		"error with bad run":     func(r *ToolCallRecord) { r.State, r.PipelineRun = "error", "-" },
	} {
		r := validToolRecord()
		mutate(&r)
		if !errors.Is(r.Validate(), ErrToolCall) {
			t.Fatal("invalid record accepted:", name)
		}
	}
}

// Invalid input is refused before any database access (no pool exists).
func TestToolCallInputRefusedBeforeDatabase(t *testing.T) {
	db := &DB{}
	ctx := context.Background()
	for _, input := range [][2]string{{"not-a-uuid", strings.Repeat("a", 64)}, {NewID(), strings.Repeat("a", 63)}, {NewID(), strings.Repeat("A", 64)}} {
		if _, err := db.BeginToolCall(ctx, input[0], input[1]); !errors.Is(err, ErrToolCall) {
			t.Fatal("invalid begin accepted", input)
		}
	}
	for _, sequence := range []int{0, MaxToolCalls + 1} {
		if err := db.FinishToolCall(ctx, NewID(), sequence, validToolRecord()); !errors.Is(err, ErrToolCall) {
			t.Fatal("invalid sequence accepted", sequence)
		}
	}
	if err := db.FinishToolCall(ctx, "x", 1, validToolRecord()); !errors.Is(err, ErrToolCall) {
		t.Fatal("invalid allocation accepted")
	}
	if err := db.FinishToolCall(ctx, NewID(), 1, ToolCallRecord{State: "started"}); !errors.Is(err, ErrToolCall) {
		t.Fatal("invalid record accepted")
	}
}

// toolCallFixture returns a claimed execution whose allocation may journal
// tool calls, the worker connection and the request id.
func toolCallFixture(t *testing.T) (*DB, *DB, ModelAllocation, string) {
	t.Helper()
	db, _, _, child := repairLedgerFixture(t)
	worker := repairRoles(t, db)["worker"]
	var request string
	if err := db.Pool.QueryRow(`SELECT request_id FROM execution_models WHERE allocation_id=$1`, child.ID).Scan(&request); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(`UPDATE execution_requests SET state='claimed',claim_token=$2,claim_until=clock_timestamp()+interval '5 minutes' WHERE id=$1`, request, NewID()); err != nil {
		t.Fatal(err)
	}
	return db, worker, child, request
}

func toolCallState(t *testing.T, db *DB, allocation string, sequence int) (string, bool) {
	t.Helper()
	var state string
	var finished bool
	if err := db.Pool.QueryRow(`SELECT state,finished_at IS NOT NULL FROM tool_calls WHERE allocation_id=$1 AND sequence=$2`, allocation, sequence).Scan(&state, &finished); err != nil {
		t.Fatal(err)
	}
	return state, finished
}

func TestToolCallJournalSequencesAndFinishesOnce(t *testing.T) {
	db, worker, a, _ := toolCallFixture(t)
	ctx := context.Background()
	if _, err := db.BeginToolCall(ctx, a.ID, strings.Repeat("d", 64)); !errors.Is(err, ErrToolCall) {
		t.Fatal("the bootstrap identity journaled a tool call", err)
	}
	for sequence := 1; sequence <= MaxToolCalls; sequence++ {
		got, err := worker.BeginToolCall(ctx, a.ID, strings.Repeat("d", 64))
		if err != nil || got != sequence {
			t.Fatal("begin", sequence, got, err)
		}
		if state, finished := toolCallState(t, db, a.ID, sequence); state != "started" || finished {
			t.Fatal("begin did not record a started call", state)
		}
		if _, err = worker.BeginToolCall(ctx, a.ID, strings.Repeat("e", 64)); !errors.Is(err, ErrToolCall) {
			t.Fatal("second call admitted while one is started", err)
		}
		record := validToolRecord()
		if sequence%2 == 0 {
			record = ToolCallRecord{Revision: strings.Repeat("a", 40), State: "error"}
		}
		if err = worker.FinishToolCall(ctx, a.ID, sequence, record); err != nil {
			t.Fatal("finish", sequence, err)
		}
		if err = worker.FinishToolCall(ctx, a.ID, sequence, validToolRecord()); !errors.Is(err, ErrToolCall) {
			t.Fatal("finished call rewritten", err)
		}
		if state, finished := toolCallState(t, db, a.ID, sequence); state != record.State || !finished {
			t.Fatal("finish not recorded", state)
		}
	}
	if _, err := worker.BeginToolCall(ctx, a.ID, strings.Repeat("d", 64)); !errors.Is(err, ErrToolCall) {
		t.Fatal("call beyond the bound admitted", err)
	}
	if err := worker.FinishToolCall(ctx, NewID(), 1, validToolRecord()); !errors.Is(err, ErrToolCall) {
		t.Fatal("unknown call finished", err)
	}
	var run, revision, tree, summary string
	if err := db.Pool.QueryRow(`SELECT pipeline_run,revision,tree,summary_sha256 FROM tool_calls WHERE allocation_id=$1 AND sequence=1`, a.ID).Scan(&run, &revision, &tree, &summary); err != nil {
		t.Fatal(err)
	}
	if want := validToolRecord(); run != want.PipelineRun || revision != want.Revision || tree != want.Tree || summary != want.SummarySHA256 {
		t.Fatal("run identity not recorded")
	}
	// The worker holds only the outcome columns: keys and digests are immutable.
	for _, statement := range []string{
		`UPDATE tool_calls SET request_sha256=repeat('f',64) WHERE allocation_id='` + a.ID + `'`,
		`UPDATE tool_calls SET sequence=sequence WHERE allocation_id='` + a.ID + `'`,
		`INSERT INTO tool_calls(allocation_id,sequence,request_sha256,state,finished_at) VALUES('` + a.ID + `',8,repeat('f',64),'passed',clock_timestamp())`,
		`DELETE FROM tool_calls WHERE allocation_id='` + a.ID + `'`,
	} {
		requireSQLState(t, func() error { _, err := worker.Pool.Exec(statement); return err }(), "42501")
	}
}

func TestToolCallRequiresALiveClaimedExecution(t *testing.T) {
	db, worker, a, request := toolCallFixture(t)
	ctx := context.Background()
	for _, setup := range []string{
		`UPDATE execution_requests SET claim_until=clock_timestamp()-interval '1 second' WHERE id=$1`,
		`UPDATE execution_requests SET state='stopped',stop_reason='aborted' WHERE id=$1`,
	} {
		if _, err := db.Pool.Exec(setup, request); err != nil {
			t.Fatal(err)
		}
		if _, err := worker.BeginToolCall(ctx, a.ID, strings.Repeat("d", 64)); !errors.Is(err, ErrToolCall) {
			t.Fatal("tool call journaled without a live claim", setup, err)
		}
	}
	// An allocation with no execution binding is refused too.
	if _, err := worker.BeginToolCall(ctx, NewID(), strings.Repeat("d", 64)); !errors.Is(err, ErrToolCall) {
		t.Fatal("unbound allocation journaled", err)
	}
	var count int
	if err := db.Pool.QueryRow(`SELECT count(*) FROM tool_calls WHERE allocation_id=$1`, a.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("refused begin wrote a row", count, err)
	}
}

func TestExecutionRecoveryAbandonsStartedToolCalls(t *testing.T) {
	for _, ending := range []string{
		// The claim expired while the tool ran.
		`UPDATE execution_requests SET claim_until=clock_timestamp()-interval '1 second' WHERE id=$1`,
		// The controller stopped its own claim but never finished the call.
		`UPDATE execution_requests SET state='stopped',stop_reason='aborted',claim_token=NULL,claim_until=NULL WHERE id=$1`,
	} {
		db, worker, a, request := toolCallFixture(t)
		ctx := context.Background()
		if _, err := worker.BeginToolCall(ctx, a.ID, strings.Repeat("d", 64)); err != nil {
			t.Fatal(err)
		}
		if err := worker.FinishToolCall(ctx, a.ID, 1, validToolRecord()); err != nil {
			t.Fatal(err)
		}
		if _, err := worker.BeginToolCall(ctx, a.ID, strings.Repeat("e", 64)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Pool.Exec(ending, request); err != nil {
			t.Fatal(err)
		}
		if err := worker.ExpireExecutionClaims(ctx); err != nil {
			t.Fatal(err)
		}
		if state, finished := toolCallState(t, db, a.ID, 2); state != "abandoned" || !finished {
			t.Fatal("started tool call not abandoned", ending, state)
		}
		if state, _ := toolCallState(t, db, a.ID, 1); state != "passed" {
			t.Fatal("recovery rewrote a finished call", state)
		}
		if err := worker.FinishToolCall(ctx, a.ID, 2, validToolRecord()); !errors.Is(err, ErrToolCall) {
			t.Fatal("abandoned call finished late", err)
		}
	}
}

func TestToolCallTableChecksBindFieldsToState(t *testing.T) {
	db, _, a, _ := toolCallFixture(t)
	insert := `INSERT INTO tool_calls(allocation_id,sequence,request_sha256,state,pipeline_run,revision,tree,summary_sha256,finished_at) VALUES($1,1,repeat('a',64),$2,$3,$4,'',$5,$6)`
	now := time.Now()
	for name, row := range map[string][]any{
		"started finished":       {"started", "", "", "", now},
		"started with outcome":   {"started", "run", strings.Repeat("a", 40), "", nil},
		"abandoned with outcome": {"abandoned", "run", "", strings.Repeat("c", 64), now},
		"abandoned unfinished":   {"abandoned", "", "", "", nil},
		"passed without summary": {"passed", "run", strings.Repeat("a", 40), "", now},
		"failed without run":     {"failed", "", strings.Repeat("a", 40), strings.Repeat("c", 64), now},
		"error unfinished":       {"error", "", "", "", nil},
		"bad run name":           {"error", "Run_1", "", "", now},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := db.Pool.Exec(insert, append([]any{a.ID}, row...)...)
			requireSQLState(t, err, "23514")
		})
	}
	if _, err := db.Pool.Exec(insert, a.ID, "error", "", "", "", now); err != nil {
		t.Fatal("error before a run refused", err)
	}
	if _, err := db.Pool.Exec(`INSERT INTO tool_calls(allocation_id,sequence,request_sha256,config_map) VALUES($1,2,repeat('a',64),'other')`, a.ID); err == nil {
		t.Fatal("a ConfigMap name outside the candidate pattern was admitted")
	} else {
		requireSQLState(t, err, "23514")
	}
}

// Object names are journaled as soon as each exists, set once, kept by
// recovery, and must match at finish.
func TestToolCallObjectsAreRecordedOnceAndKept(t *testing.T) {
	db, worker, a, request := toolCallFixture(t)
	ctx := context.Background()
	cm := "ai-sdlc-candidate-0123456789abcdef-abc123"
	if _, err := worker.BeginToolCall(ctx, a.ID, strings.Repeat("d", 64)); err != nil {
		t.Fatal(err)
	}
	if err := worker.RecordToolCallObjects(ctx, a.ID, 1, "", cm); err != nil {
		t.Fatal("config map", err)
	}
	if err := worker.RecordToolCallObjects(ctx, a.ID, 1, "build-test-x1", cm); err != nil {
		t.Fatal("pipeline run", err)
	}
	for _, change := range [][2]string{{"build-test-x2", cm}, {"", "ai-sdlc-candidate-0123456789abcdef-zzz999"}, {"", ""}} {
		if err := worker.RecordToolCallObjects(ctx, a.ID, 1, change[0], change[1]); !errors.Is(err, ErrToolCall) {
			t.Fatal("recorded name changed", change, err)
		}
	}
	other := validToolRecord()
	other.PipelineRun = "build-test-x2"
	if err := worker.FinishToolCall(ctx, a.ID, 1, other); !errors.Is(err, ErrToolCall) {
		t.Fatal("finish named another run", err)
	}
	if _, err := db.Pool.Exec(`UPDATE execution_requests SET claim_until=clock_timestamp()-interval '1 second' WHERE id=$1`, request); err != nil {
		t.Fatal(err)
	}
	if err := worker.ExpireExecutionClaims(ctx); err != nil {
		t.Fatal(err)
	}
	var state, run, configMap string
	if err := db.Pool.QueryRow(`SELECT state,pipeline_run,config_map FROM tool_calls WHERE allocation_id=$1 AND sequence=1`, a.ID).Scan(&state, &run, &configMap); err != nil ||
		state != "abandoned" || run != "build-test-x1" || configMap != cm {
		t.Fatal("abandoned call lost its object names", state, run, configMap, err)
	}
}

func TestToolCallPrivilegesFollowExecutionMode(t *testing.T) {
	db, worker, _, _ := toolCallFixture(t)
	passwords := map[string]string{}
	for component := range componentTables {
		passwords[component] = strings.ReplaceAll(component, "-", "_") + strings.Repeat("x", 40)
	}
	var count int
	if err := worker.Pool.QueryRow(`SELECT count(*) FROM tool_calls`).Scan(&count); err != nil {
		t.Fatal("verification mode withheld the journal", err)
	}
	for _, mode := range []string{"maintenance", "shadow"} {
		if err := db.ProvisionRolesForMode(context.Background(), passwords, mode); err != nil {
			t.Fatal(err)
		}
		requireSQLState(t, worker.Pool.QueryRow(`SELECT count(*) FROM tool_calls`).Scan(&count), "42501")
		var columns int
		if err := db.Pool.QueryRow(`SELECT count(*) FROM information_schema.column_privileges WHERE grantee='aisdlc_worker' AND table_name='tool_calls'`).Scan(&columns); err != nil || columns != 0 {
			t.Fatal("mode kept tool journal column grants", mode, columns, err)
		}
	}
	if err := db.ProvisionRolesForMode(context.Background(), passwords, "candidate"); err != nil {
		t.Fatal(err)
	}
	if err := worker.Pool.QueryRow(`SELECT count(*) FROM tool_calls`).Scan(&count); err != nil {
		t.Fatal("candidate mode withheld the journal", err)
	}
}
