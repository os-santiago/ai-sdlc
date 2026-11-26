package flowconsole

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/flowevent"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

// These tests run against a migrated disposable PostgreSQL
// (AI_SDLC_TEST_DATABASE_URL) and read through the provisioned
// aisdlc_flow_console role, so they exercise the migration's policies, the
// column grants and the derivation together.

var componentPasswords = map[string]string{}

func init() {
	for _, c := range []string{"intake", "admission-controller", "orchestrator", "worker", "verifier", "release-manager", "relay", "deployer", "build-controller"} {
		componentPasswords[c] = strings.ReplaceAll(c, "-", "_") + strings.Repeat("q", 40)
	}
}

const consolePassword = "flow_console" + "qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq"

type journalFixture struct {
	admin, console *store.DB
	repository     string
	now            time.Time
}

func newJournalFixture(t *testing.T) *journalFixture {
	t.Helper()
	connection := os.Getenv("AI_SDLC_TEST_DATABASE_URL")
	if connection == "" {
		t.Skip("disposable PostgreSQL required")
	}
	ctx := context.Background()
	admin, err := store.Open(ctx, connection)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	if err = admin.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	passwords := map[string]string{store.FlowConsoleComponent: consolePassword}
	for k, v := range componentPasswords {
		passwords[k] = v
	}
	if err = admin.ProvisionRoles(ctx, passwords); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(connection)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(store.FlowConsoleRole, consolePassword)
	console, err := store.Open(ctx, u.String())
	if err != nil {
		t.Fatal("flow console role connection failed")
	}
	t.Cleanup(func() { console.Close() })
	var now time.Time
	if err = admin.Pool.QueryRow(`SELECT date_trunc('second', now())`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	return &journalFixture{admin: admin, console: console, repository: "flowtest/run-" + strings.ReplaceAll(store.NewID(), "-", "")[:12], now: now.UTC()}
}

func digest(parts ...any) string {
	sum := sha256.Sum256([]byte(fmt.Sprint(parts...)))
	return hex.EncodeToString(sum[:])
}

// seed inserts synthetic journal rows as the bootstrap owner. Foreign keys
// to aggregates this test does not exercise (budgets, verification parents)
// are skipped with the session replication role; CHECK constraints still
// apply, so every row has a valid shape.
func (f *journalFixture) seed(t *testing.T, statements ...string) {
	t.Helper()
	tx, err := f.admin.Pool.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	for _, s := range statements {
		if _, err = tx.Exec(s); err != nil {
			t.Fatalf("seed: %v\n%s", err, s)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func (f *journalFixture) at(minutes float64) string {
	return "'" + f.now.Add(time.Duration(minutes*float64(time.Minute))).Format(time.RFC3339Nano) + "'::timestamptz"
}

func (f *journalFixture) workItem(t *testing.T, issue int, status string) string {
	plan, item := store.NewID(), store.NewID()
	f.seed(t,
		fmt.Sprintf(`INSERT INTO plans(id,repository,correlation_id,created_at,updated_at) VALUES('%s','%s','flow-test',%s,%s)`, plan, f.repository, f.at(-60), f.at(-60)),
		fmt.Sprintf(`INSERT INTO work_items(plan_id,id,uuid,issue_numbers,acceptance_criteria,status,created_at,updated_at) VALUES('%s','item','%s','[%d]','["criterion"]','%s',%s,%s)`,
			plan, item, issue, status, f.at(-59), f.at(-59)))
	return item
}

// execution inserts a request with its lifecycle; state, reason and claim
// are caller-chosen.
func (f *journalFixture) execution(t *testing.T, item string, attempt int, state, reason string, created float64, claim string, extra ...string) string {
	id := "execution-" + digest(item, attempt)
	token, until := "NULL", "NULL"
	if claim != "" {
		token, until = "'"+store.NewID()+"'", claim
	}
	statements := []string{fmt.Sprintf(`INSERT INTO execution_requests(id,work_item_uuid,attempt,repository,authority_revision,payload_digest,payload,receipt,expires_at,state,claim_token,claim_until,stop_reason,created_at,updated_at)
 VALUES('%s','%s',%d,'%s','%s','%s','\x01','\x01',%s,'%s',%s,%s,'%s',%s,%s)`, id, item, attempt, f.repository, digest("rev"), digest(id, "payload"), f.at(created+60), state, token, until, reason, f.at(created), f.at(created+20))}
	return id + "|" + strings.Join(append(statements, extra...), "\n;\n")
}

func (f *journalFixture) lifecycle(id string, phase, cleanup string, attempts int, created float64) string {
	short := digest(id)[:32]
	return fmt.Sprintf(`INSERT INTO execution_lifecycles(request_id,claim_token,claim_until,binding_digest,source_digest,source_bytes,namespace,pod_name,config_name,phase,created_at,updated_at,cleanup_state,cleanup_attempts)
 VALUES('%s','%s',%s,'%s','%s',10,'ai-sdlc-exec','executor-%s-1','assignment-%s','%s',%s,%s,'%s',%d)`,
		id, store.NewID(), f.at(created+30), digest(id, "binding"), digest(id, "source"), store.NewID(), short, phase, f.at(created), f.at(created+21), cleanup, attempts)
}

func split(t *testing.T, s string) (string, []string) {
	parts := strings.SplitN(s, "|", 2)
	return parts[0], strings.Split(parts[1], "\n;\n")
}

func (f *journalFixture) derive(t *testing.T) map[string][]flowevent.Event {
	t.Helper()
	j, err := store.ReadFlowJournal(context.Background(), f.console, f.now.Add(-24*time.Hour), 5000)
	if err != nil {
		t.Fatal("console read failed", err)
	}
	out := map[string][]flowevent.Event{}
	for _, e := range Derive(j, DeriveOptions{StaleWait: 10 * time.Minute}) {
		if e.Repository != f.repository {
			continue
		}
		if err := e.Validate(); err != nil {
			t.Fatal("derived event outside the contract", err)
		}
		out[e.Stage] = append(out[e.Stage], e)
	}
	return out
}

func find(events []flowevent.Event, kind, step, diagnostic string) *flowevent.Event {
	for i, e := range events {
		if e.Kind == kind && (step == "" || e.Step == step) && e.Diagnostic == diagnostic {
			return &events[i]
		}
	}
	return nil
}

func must(t *testing.T, events []flowevent.Event, kind, step, diagnostic string) flowevent.Event {
	t.Helper()
	e := find(events, kind, step, diagnostic)
	if e == nil {
		t.Fatalf("missing %s/%s/%s in %+v", kind, step, diagnostic, events)
	}
	return *e
}

func TestJournalDerivesIntakeAdmissionAndPlanning(t *testing.T) {
	f := newJournalFixture(t)
	f.workItem(t, 501, "ready")
	f.seed(t,
		// Two revisions: the console shows each, not only the latest.
		fmt.Sprintf(`INSERT INTO intake_revisions(repository,issue_number,revision,generation,config_digest,event_id,payload,observed_at) VALUES('%s',501,'r2',2,'%s','%s','\x01',%s)`,
			f.repository, digest(1), digest(f.repository, 2), f.at(-62)),
		fmt.Sprintf(`INSERT INTO intake_revisions(repository,issue_number,revision,generation,config_digest,event_id,payload,observed_at) VALUES('%s',501,'r3',3,'%s','%s','\x01',%s)`,
			f.repository, digest(1), digest(f.repository, 3), f.at(-61)),
		fmt.Sprintf(`INSERT INTO admissions(repository,issue_number,issue_revision,decision,risk,policy_version,correlation_id,created_at,updated_at) VALUES('%s',501,'r1:3','accepted','low','v1','c',%s,%s)`, f.repository, f.at(-60.5), f.at(-60.4)),
		fmt.Sprintf(`INSERT INTO admissions(repository,issue_number,issue_revision,decision,risk,policy_version,correlation_id,created_at,updated_at) VALUES('%s',506,'r1:1','needs-clarification','medium','v1','c',%s,%s)`, f.repository, f.at(-30), f.at(-30)),
		fmt.Sprintf(`INSERT INTO admissions(repository,issue_number,issue_revision,decision,risk,policy_version,correlation_id,created_at,updated_at) VALUES('%s',507,'r1:1','rejected','high','v1','c',%s,%s)`, f.repository, f.at(-29), f.at(-29)))
	events := f.derive(t)
	if len(events["intake"]) != 2 {
		t.Fatalf("intake revisions %+v", events["intake"])
	}
	intake := events["intake"][1]
	if intake.Kind != flowevent.KindCompleted || intake.Step != "observed" || intake.Issue != 501 || intake.Attributes["generation"] != "3" || intake.Component != "intake" {
		t.Fatalf("intake %+v", intake)
	}
	accepted := must(t, events["admission"], flowevent.KindCompleted, "decision", "")
	if accepted.Issue != 501 || accepted.Attributes["risk"] != "low" || accepted.DurationMs != 6000 {
		t.Fatalf("admission %+v", accepted)
	}
	if must(t, events["admission"], flowevent.KindBlocked, "decision", "needs-clarification").Issue != 506 {
		t.Fatal("person-blocked admission")
	}
	must(t, events["admission"], flowevent.KindFailed, "decision", "rejected")
	planned := must(t, events["planning"], flowevent.KindCompleted, "work-item", "")
	if planned.Issue != 501 || planned.Component != "orchestrator" || planned.Attributes["status"] != "ready" {
		t.Fatalf("planning %+v", planned)
	}
}

func TestJournalDerivesExecutionCandidateModelCallsAndDegradation(t *testing.T) {
	f := newJournalFixture(t)
	item := f.workItem(t, 511, "ready")
	id, rows := split(t, f.execution(t, item, 1, "stopped", "candidate-captured", -50, ""))
	allocation := store.NewID()
	rows = append(rows, f.lifecycle(id, "candidate-captured", "complete", 1, -49.9),
		fmt.Sprintf(`INSERT INTO execution_effects(request_id,kind,token,result,created_at,completed_at) VALUES('%s','config','%s','\x01',%s,%s)`, id, store.NewID(), f.at(-49.8), f.at(-49.7)),
		fmt.Sprintf(`INSERT INTO execution_effects(request_id,kind,token,result,created_at,completed_at) VALUES('%s','start','%s',NULL,%s,NULL)`, id, store.NewID(), f.at(-49.6)),
		fmt.Sprintf(`INSERT INTO model_allocations(id,work_item_uuid,attempt,pod_uid,max_calls,max_input_bytes,max_output_bytes,response_limit,expires_at) VALUES('%s','%s',1,'%s',32,4194304,8388608,262144,%s)`, allocation, item, store.NewID(), f.at(10)),
		fmt.Sprintf(`INSERT INTO execution_models(request_id,allocation_id,config_digest,created_at) VALUES('%s','%s','%s',%s)`, id, allocation, digest("cfg"), f.at(-49.5)),
		fmt.Sprintf(`INSERT INTO model_calls(allocation_id,sequence,request_sha256,input_bytes,response_limit,expires_at,state,output_bytes,response_sha256,response_status,response_handed_off_at) VALUES('%s',1,'%s',1000,262144,%s,'completed',10,'%s',200,%s)`, allocation, digest(1), f.at(10), digest(2), f.at(-49)),
		fmt.Sprintf(`INSERT INTO model_calls(allocation_id,sequence,request_sha256,input_bytes,response_limit,expires_at,state,output_bytes,response_sha256,response_status,response_handed_off_at) VALUES('%s',2,'%s',2000,262144,%s,'completed',20,'%s',200,%s)`, allocation, digest(3), f.at(10), digest(4), f.at(-48)),
		fmt.Sprintf(`INSERT INTO model_calls(allocation_id,sequence,request_sha256,input_bytes,response_limit,expires_at,state,output_bytes,response_sha256,response_status) VALUES('%s',3,'%s',3000,262144,%s,'ambiguous',0,'%s',503)`, allocation, digest(5), f.at(10), digest(6)),
		fmt.Sprintf(`INSERT INTO execution_candidates(request_id,allocation_id,pod_uid,source_sha,source_tree_sha256,candidate_tree_sha256,export_sha256,export_bytes,delta_sha256,delta,changes,model_calls,created_at)
 VALUES('%s','%s','%s','%s','%s','%s','%s',10,'%s','\x01',3,2,%s)`, id, allocation, store.NewID(), digest(7)[:40], digest(8), digest(9), digest(10), digest(11), f.at(-47)))
	f.seed(t, rows...)
	events := f.derive(t)["execution"]
	waiting := must(t, events, flowevent.KindWaiting, "queued", "")
	if waiting.Attempt != 1 || waiting.Issue != 511 || waiting.WorkItem != item || waiting.Attributes["waitedMs"] != "6000" {
		t.Fatalf("waiting %+v", waiting)
	}
	must(t, events, flowevent.KindStarted, "claim", "")
	if config := must(t, events, flowevent.KindProgressed, "config", ""); config.DurationMs != 6000 {
		t.Fatalf("effect duration %+v", config)
	}
	if find(events, flowevent.KindProgressed, "start", "") != nil {
		t.Fatal("unacknowledged effect reported as progress")
	}
	var calls []flowevent.Event
	for _, e := range events {
		if e.Step == "model-call" && e.Kind == flowevent.KindProgressed {
			calls = append(calls, e)
		}
	}
	// Generator calls: the first from the model allocation, the second from the first handoff.
	if len(calls) != 2 || calls[0].LatencyMs != 30000 || calls[1].LatencyMs != 60000 || calls[1].Attributes["inputBytes"] != "2000" {
		t.Fatalf("model calls %+v", calls)
	}
	must(t, events, flowevent.KindDegraded, "model-call", "model-status-503")
	done := must(t, events, flowevent.KindCompleted, "candidate", "")
	if done.Attributes["changes"] != "3" || done.Attributes["modelCalls"] != "2" || done.DurationMs != int64(2.9*60000) {
		t.Fatalf("candidate %+v", done)
	}
	if find(events, flowevent.KindRetried, "", "") != nil {
		t.Fatal("first attempt reported as retry")
	}
}

func TestJournalDerivesSafeStopsRetriesStaleWaitsLeasesAndQuarantine(t *testing.T) {
	f := newJournalFixture(t)
	item := f.workItem(t, 521, "ready")
	first, rows := split(t, f.execution(t, item, 1, "stopped", "uncertain", -55, ""))
	rows = append(rows, f.lifecycle(first, "start-ready", "uncertain", 3, -54.9),
		fmt.Sprintf(`INSERT INTO execution_failures(request_id,pod_uid,container_id,exit_code,observed_at) VALUES('%s','%s','containerd://%s',22,%s)`, first, store.NewID(), digest(1)[:32], f.at(-36)))
	_, second := split(t, f.execution(t, item, 2, "queued", "", -25, ""))
	other := f.workItem(t, 522, "ready")
	lapsed, third := split(t, f.execution(t, other, 1, "claimed", "", -15, f.at(-2)))
	third = append(third, f.lifecycle(lapsed, "start-ready", "pending", 0, -14.9))
	f.seed(t, append(append(rows, second...), third...)...)
	events := f.derive(t)["execution"]
	stop := must(t, events, flowevent.KindStoppedSafely, "stop", "worker-exit-22")
	if stop.Attempt != 1 || stop.Attributes["phase"] != "start-ready" || stop.Attributes["reason"] != "uncertain" || stop.Attributes["exitCode"] != "22" {
		t.Fatalf("stop %+v", stop)
	}
	if q := must(t, events, flowevent.KindQuarantined, "cleanup", "cleanup-uncertain"); q.Attributes["cleanupAttempts"] != "3" {
		t.Fatalf("quarantine %+v", q)
	}
	retried := must(t, events, flowevent.KindRetried, "attempt", "")
	if retried.Attempt != 2 || retried.Attributes["previousAttempt"] != "1" {
		t.Fatalf("retry %+v", retried)
	}
	stale := must(t, events, flowevent.KindWaiting, "queued", "stale-wait")
	if stale.Attempt != 2 || !stale.Time.Equal(f.now.Add(-15*time.Minute)) {
		t.Fatalf("stale wait %+v", stale)
	}
	blocked := must(t, events, flowevent.KindBlocked, "lease", "lease-lapsed")
	if blocked.Issue != 522 || !blocked.Time.Equal(f.now.Add(-2*time.Minute)) {
		t.Fatalf("lease %+v", blocked)
	}
}

func TestJournalDerivesVerificationReviewPublicationAndMerge(t *testing.T) {
	f := newJournalFixture(t)
	item := f.workItem(t, 531, "ready")
	id, rows := split(t, f.execution(t, item, 1, "stopped", "candidate-captured", -58, ""))
	verification, review, publication, merge := "verification-"+digest("v", item), "review-"+digest("r", item), "publication-"+digest("p", item), "merge-"+digest("m", item)
	rows = append(rows,
		fmt.Sprintf(`INSERT INTO verification_requests(id,candidate_id,payload_digest,payload,receipt,expires_at,state,claim_token,claim_until,cleanup_state,proof,created_at)
 VALUES('%s','%s','%s','\x01','\x01',%s,'passed','%s',%s,'complete','\x01',%s)`, verification, id, digest(1), f.at(30), store.NewID(), f.at(0), f.at(-40)),
		fmt.Sprintf(`INSERT INTO verification_effects(request_id,kind,token,resource_name,resource_uid,created_at,completed_at) VALUES('%s','tests','%s','verify-%s-tests','%s',%s,%s)`, verification, store.NewID(), digest(2)[:32], store.NewID(), f.at(-39), f.at(-35)),
		fmt.Sprintf(`INSERT INTO review_runs(id,verification_id,candidate_id,work_item_uuid,behavior_digest,profile_digest,configuration_digest,assignment_digest,expires_at,state,claim_token,claim_until,cleanup_state,proof,created_at)
 VALUES('%s','%s','%s','%s','%s','%s','%s','%s',%s,'passed','%s',%s,'complete','\x01',%s)`, review, verification, id, item, digest(3), digest(4), digest(5), digest(6), f.at(30), store.NewID(), f.at(0), f.at(-34)),
		fmt.Sprintf(`INSERT INTO review_effects(run_id,kind,token,resource_name,resource_uid,created_at,completed_at) VALUES('%s','pod','%s','review-pod','%s',%s,%s)`, review, store.NewID(), store.NewID(), f.at(-33), f.at(-32.5)),
		fmt.Sprintf(`INSERT INTO review_model_calls(run_id,sequence,token,pod_uid,request_digest,input_bytes,state,response_digest,output_bytes,observation,handed_off_at,created_at)
 VALUES('%s',1,'%s','%s','%s',5000,'handed-off','%s',100,'\x01',%s,%s)`, review, store.NewID(), store.NewID(), digest(7), digest(8), f.at(-31), f.at(-32)),
		fmt.Sprintf(`INSERT INTO publication_requests(id,review_id,verification_id,candidate_id,repository,payload_digest,payload,receipt,expires_at,state,claim_token,claim_until,plan_digest,commit_time,proof,created_at)
 VALUES('%s','%s','%s','%s','%s','%s','\x01','\x01',%s,'published','%s',%s,'%s',%s,'\x01',%s)`, publication, review, verification, id, f.repository, digest(9), f.at(30), store.NewID(), f.at(0), digest(10), f.at(-29), f.at(-30)),
		fmt.Sprintf(`INSERT INTO publication_effects(request_id,kind,token,request_digest,receipt,receipt_digest,created_at,completed_at) VALUES('%s','pull-request','%s','%s','\x01','%s',%s,%s)`, publication, store.NewID(), digest(11), digest(12), f.at(-29), f.at(-28.5)),
		fmt.Sprintf(`INSERT INTO published_records(publication_id,payload_digest,payload,receipt,proof_digest,proof,effects_digest,effects,authority_expires_at,published_at)
 VALUES('%s','%s','\x01','\x01','%s','\x01','%s','\x01',%s,%s)`, publication, digest(13), digest(14), digest(15), f.at(30), f.at(-28)),
		fmt.Sprintf(`INSERT INTO merge_requests(id,publication_id,record_digest,gate_digest,authority_revision,payload_digest,payload,receipt,gate,policy,expires_at,state,claim_token,claim_until,proof,merged_at,created_at)
 VALUES('%s','%s','%s','%s','%s','%s','\x01','\x01','\x01','\x01',%s,'merged','%s',%s,'\x01',%s,%s)`, merge, publication, digest(16), digest(17), digest(18), digest(19), f.at(30), store.NewID(), f.at(0), f.at(-20), f.at(-27)),
		fmt.Sprintf(`INSERT INTO merge_effects(request_id,token,request_digest,receipt,receipt_digest,created_at,completed_at) VALUES('%s','%s','%s','\x01','%s',%s,%s)`, merge, store.NewID(), digest(20), digest(21), f.at(-21), f.at(-20.5)))
	// A refused candidate and a quarantined publication on a second item.
	refused := f.workItem(t, 532, "ready")
	rid, rrows := split(t, f.execution(t, refused, 1, "stopped", "candidate-captured", -45, ""))
	rverification, rpublication := "verification-"+digest("v", refused), "publication-"+digest("p", refused)
	rrows = append(rrows,
		fmt.Sprintf(`INSERT INTO verification_requests(id,candidate_id,payload_digest,payload,receipt,expires_at,state,claim_token,claim_until,reason,cleanup_state,created_at)
 VALUES('%s','%s','%s','\x01','\x01',%s,'failed','%s',%s,'comparison-failed','uncertain',%s)`, rverification, rid, digest(22), f.at(30), store.NewID(), f.at(0), f.at(-30)),
		fmt.Sprintf(`INSERT INTO verification_effects(request_id,kind,token,resource_name,created_at,not_sent_at) VALUES('%s','tests','%s','verify-%s-tests',%s,%s)`, rverification, store.NewID(), digest(23)[:32], f.at(-29), f.at(-26)),
		fmt.Sprintf(`INSERT INTO publication_requests(id,review_id,verification_id,candidate_id,repository,payload_digest,payload,receipt,expires_at,state,claim_token,claim_until,plan_digest,commit_time,reason,created_at)
 VALUES('%s','review-%s','%s','%s','%s','%s','\x01','\x01',%s,'failed','%s',%s,'%s',%s,'uncertain',%s)`, rpublication, digest("r", refused), rverification, rid, f.repository, digest(25), f.at(30), store.NewID(), f.at(0), digest(26), f.at(-24), f.at(-25)),
		fmt.Sprintf(`INSERT INTO publication_effects(request_id,kind,token,request_digest,created_at) VALUES('%s','pull-request','%s','%s',%s)`, rpublication, store.NewID(), digest(27), f.at(-23)))
	f.seed(t, append(rows, rrows...)...)
	events := f.derive(t)

	passed := must(t, events["verification"], flowevent.KindCompleted, "verdict", "")
	if passed.Issue != 531 || passed.Component != "verifier" || !passed.Time.Equal(f.now.Add(-35*time.Minute)) || passed.DurationMs != 4*60000 {
		t.Fatalf("verification %+v", passed)
	}
	refusal := must(t, events["verification"], flowevent.KindFailed, "verdict", "comparison-failed")
	if refusal.Issue != 532 || !refusal.Time.Equal(f.now.Add(-26*time.Minute)) {
		t.Fatalf("refusal %+v", refusal)
	}
	must(t, events["verification"], flowevent.KindQuarantined, "cleanup", "cleanup-uncertain")
	call := must(t, events["review"], flowevent.KindProgressed, "model-call", "")
	if call.LatencyMs != 60000 || call.Attributes["inputBytes"] != "5000" {
		t.Fatalf("review model call %+v", call)
	}
	if r := must(t, events["review"], flowevent.KindCompleted, "verdict", ""); !r.Time.Equal(f.now.Add(-31 * time.Minute)) {
		t.Fatalf("review %+v", r)
	}
	published := must(t, events["publication"], flowevent.KindCompleted, "verdict", "")
	if !published.Time.Equal(f.now.Add(-28*time.Minute)) || published.Component != "release-manager" {
		t.Fatalf("publication %+v", published)
	}
	must(t, events["publication"], flowevent.KindProgressed, "pull-request", "")
	must(t, events["publication"], flowevent.KindStoppedSafely, "verdict", "uncertain")
	if q := must(t, events["publication"], flowevent.KindQuarantined, "pull-request", "unresolved-effect"); q.Issue != 532 {
		t.Fatalf("publication quarantine %+v", q)
	}
	merged := must(t, events["merge"], flowevent.KindCompleted, "verdict", "")
	if !merged.Time.Equal(f.now.Add(-20*time.Minute)) || merged.Issue != 531 || merged.Attempt != 1 {
		t.Fatalf("merge %+v", merged)
	}
	must(t, events["merge"], flowevent.KindProgressed, "merge", "")
}

// The console role reads timeline columns only and writes nothing.
func TestFlowConsoleRoleIsReadOnlyAndColumnScoped(t *testing.T) {
	f := newJournalFixture(t)
	ctx := context.Background()
	if err := store.AssertFlowConsole(ctx, f.console.Pool); err != nil {
		t.Fatal("console identity rejected", err)
	}
	if store.AssertFlowConsole(ctx, f.admin.Pool) == nil {
		t.Fatal("bootstrap owner accepted as the flow console")
	}
	if _, err := store.ReadFlowJournal(ctx, f.admin, f.now.Add(-time.Hour), 10); err == nil {
		t.Fatal("journal read accepted a non-console identity")
	}
	item := f.workItem(t, 541, "ready")
	_, rows := split(t, f.execution(t, item, 1, "queued", "", -5, ""))
	f.seed(t, rows...)
	denied := []string{
		`SELECT payload FROM execution_requests LIMIT 1`,
		`SELECT receipt FROM execution_requests LIMIT 1`,
		`SELECT claim_token FROM execution_requests LIMIT 1`,
		`SELECT * FROM execution_requests LIMIT 1`,
		`SELECT acceptance_criteria FROM work_items LIMIT 1`,
		`SELECT correlation_id FROM plans LIMIT 1`,
		`SELECT reasons FROM admissions LIMIT 1`,
		`SELECT delta FROM execution_candidates LIMIT 1`,
		`SELECT request_sha256 FROM model_calls LIMIT 1`,
		`SELECT observation FROM review_model_calls LIMIT 1`,
		`SELECT proof FROM published_records LIMIT 1`,
		`SELECT payload FROM intake_revisions LIMIT 1`,
		`SELECT revision FROM intake_revisions LIMIT 1`,
		`SELECT content_key FROM intake_observations LIMIT 1`,
		`SELECT decision FROM review_feedback LIMIT 1`,
		`SELECT payload FROM authorized_outbox LIMIT 1`,
		`SELECT container_id FROM execution_failures LIMIT 1`,
		`INSERT INTO plans(id,repository,correlation_id) VALUES(gen_random_uuid(),'a/b','c')`,
		`UPDATE execution_requests SET state='stopped', stop_reason='aborted'`,
		`UPDATE work_items SET status=status`,
		`DELETE FROM execution_requests`,
		`DELETE FROM work_items`,
		`TRUNCATE work_items`,
		`CREATE TABLE flow_console_probe(x int)`,
	}
	for _, statement := range denied {
		if _, err := f.console.Pool.ExecContext(ctx, statement); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("console statement not denied: %s (%v)", statement, err)
		}
	}
	var visible int
	if err := f.console.Pool.QueryRowContext(ctx, `SELECT count(*) FROM execution_requests WHERE work_item_uuid=$1`, item).Scan(&visible); err != nil || visible != 1 {
		t.Fatal("forced row security hid the timeline rows from the console", err)
	}
	// Without its credential the observer is disabled, never left with access.
	if err := f.admin.ProvisionRoles(ctx, componentPasswords); err != nil {
		t.Fatal(err)
	}
	var login bool
	var grants int
	if err := f.admin.Pool.QueryRow(`SELECT rolcanlogin, (SELECT count(*) FROM information_schema.column_privileges WHERE grantee=$1) FROM pg_roles WHERE rolname=$1`, store.FlowConsoleRole).Scan(&login, &grants); err != nil || login || grants != 0 {
		t.Fatal("disabled observer kept login or privileges", login, grants, err)
	}
	if _, err := store.ReadFlowJournal(ctx, f.console, f.now.Add(-time.Hour), 10); err == nil {
		t.Fatal("disabled observer still reads the journal")
	}
}
