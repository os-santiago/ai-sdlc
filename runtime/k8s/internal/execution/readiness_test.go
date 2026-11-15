package execution

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/intake"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/normalizedevent"
)

func (f *fixture) readyPolicy() (authority.Policy, intake.Config, error) {
	return f.Material.Policy, f.Config, nil
}

// Isolated fixture facts only. No production intake or transition uses this
// helper; operational code must obtain these rows through signed components.
func (f *fixture) seedReadiness(t *testing.T) {
	t.Helper()
	if _, err := f.Admin.Pool.Exec(`TRUNCATE intake_observations,intake_heads,intake_revisions,intake_cursors`); err != nil {
		t.Fatal(err)
	}
	snapshot := f.Snapshot(1, true)
	admission := intake.Admission{Snapshot: snapshot, Decision: "accepted", Risk: "low", Reasons: []string{"Fixture admission."}, RequiredGates: []string{"tests", "quality", "security", "issue-coverage"}, PolicyVersion: "1.0.0"}
	observed, _ := json.Marshal(snapshot)
	decided, _ := json.Marshal(admission)
	criteria, _ := json.Marshal(normalizedevent.ExtractAcceptanceCriteria(snapshot.Issue.Body))
	reasons, _ := json.Marshal(admission.Reasons)
	gates, _ := json.Marshal(admission.RequiredGates)
	contentKey := authority.Digest([]byte(snapshot.Revision + "\x00true\x00" + snapshot.ConfigDigest + "\x00" + snapshot.AuthorityRevision))
	statements := []struct {
		sql  string
		args []any
	}{
		{`UPDATE plans SET correlation_id=$1`, []any{snapshot.ID()}},
		{`UPDATE work_items SET acceptance_criteria=$1,depends_on='[]'`, []any{criteria}},
		{`INSERT INTO intake_heads(repository,issue_number,content_key,generation) VALUES($1,$2,$3,$4)`, []any{snapshot.Repository, snapshot.Issue.Number, contentKey, snapshot.Generation}},
		{`INSERT INTO intake_revisions(repository,issue_number,revision,generation,config_digest,event_id,payload) VALUES($1,$2,$3,$4,$5,$6,$7)`, []any{snapshot.Repository, snapshot.Issue.Number, snapshot.Revision, snapshot.Generation, snapshot.ConfigDigest, snapshot.ID(), observed}},
		{`INSERT INTO intake_observations(repository,issue_number,generation,config_digest,authority_revision,content_key,observed_at) VALUES($1,$2,$3,$4,$5,$6,clock_timestamp())`, []any{snapshot.Repository, snapshot.Issue.Number, snapshot.Generation, snapshot.ConfigDigest, snapshot.AuthorityRevision, contentKey}},
		{`INSERT INTO intake_cursors(repository,config_digest,watermark) VALUES($1,$2,clock_timestamp())`, []any{snapshot.Repository, snapshot.ConfigDigest}},
		{`INSERT INTO admission_heads(repository,issue_number,generation,event_id,payload) VALUES($1,$2,$3,$4,$5)`, []any{snapshot.Repository, snapshot.Issue.Number, snapshot.Generation, snapshot.ID(), observed}},
		{`INSERT INTO planning_heads(repository,issue_number,generation,event_id,payload) VALUES($1,$2,$3,$4,$5)`, []any{snapshot.Repository, snapshot.Issue.Number, snapshot.Generation, admission.ID(), decided}},
		{`INSERT INTO admissions(repository,issue_number,issue_revision,decision,risk,policy_version,reasons,required_gates,correlation_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, []any{snapshot.Repository, snapshot.Issue.Number, snapshot.Revision + ":" + strconv.FormatInt(snapshot.Generation, 10), admission.Decision, admission.Risk, admission.PolicyVersion, reasons, gates, snapshot.ID()}},
	}
	for _, statement := range statements {
		if _, err := f.Admin.Pool.Exec(statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	_, err := f.Admin.Pool.Exec(`
        GRANT SELECT (id,repository,correlation_id) ON plans TO aisdlc_worker;
        GRANT SELECT (uuid,plan_id,status,issue_numbers,acceptance_criteria,depends_on) ON work_items TO aisdlc_worker;
        GRANT SELECT (repository,issue_number,generation,content_key) ON intake_heads TO aisdlc_worker;
        GRANT SELECT (repository,issue_number,generation,event_id,revision,config_digest,payload) ON intake_revisions TO aisdlc_worker;
        GRANT SELECT (repository,config_digest,watermark) ON intake_cursors TO aisdlc_worker;
 GRANT SELECT (repository,issue_number,generation,config_digest,authority_revision,content_key,observed_at) ON intake_observations TO aisdlc_worker;
        GRANT SELECT (repository,issue_number,generation,event_id,payload) ON admission_heads,planning_heads TO aisdlc_worker;
        GRANT SELECT (repository,issue_number,issue_revision,decision,risk,policy_version,correlation_id,reasons,required_gates) ON admissions TO aisdlc_worker`)
	if err != nil {
		t.Fatal(err)
	}
	f.command.AdmissionID = admission.ID()
	f.command.SourceSHA = snapshot.BaseSHA
}

func TestExecutionReadinessRejectsChangedDurableProof(t *testing.T) {
	cases := map[string]string{
		"cross-repository-work-item":           `UPDATE plans SET repository='other/product'`,
		"wrong-plan-correlation":               `UPDATE plans SET correlation_id='unrelated'`,
		"wrong-issue":                          `UPDATE work_items SET issue_numbers='[4]'`,
		"multiple-issues":                      `UPDATE work_items SET issue_numbers='[3,4]'`,
		"changed-criteria":                     `UPDATE work_items SET acceptance_criteria='["Different scope"]'`,
		"blocked":                              `UPDATE work_items SET status='blocked'`,
		"terminal":                             `UPDATE work_items SET status='completed'`,
		"dependencies":                         `UPDATE work_items SET depends_on='["other-item"]'`,
		"null-dependencies":                    `UPDATE work_items SET depends_on='null'`,
		"omitted-issue-with-fresh-global-scan": `UPDATE intake_observations SET observed_at=clock_timestamp()-interval '10 minutes'`,
		"observation-wrong-authority":          `UPDATE intake_observations SET authority_revision=repeat('d',64)`,
		"observation-wrong-content":            `UPDATE intake_observations SET content_key=repeat('d',64)`,
		"observation-missing":                  `DELETE FROM intake_observations`,
		"stale-poller":                         `UPDATE intake_cursors SET watermark=clock_timestamp()-interval '10 minutes'`,
		"missing-poll-proof":                   `UPDATE intake_cursors SET watermark=NULL`,
		"future-poll-proof":                    `UPDATE intake_cursors SET watermark=clock_timestamp()+interval '1 hour'`,
		"newer-intake":                         `UPDATE intake_heads SET generation=2`,
		"newer-admission":                      `UPDATE admission_heads SET generation=2`,
		"newer-plan":                           `UPDATE planning_heads SET generation=2`,
		"different-intake-key":                 `UPDATE intake_heads SET content_key=repeat('d',64)`,
		"withdrawn-admission":                  `UPDATE admissions SET decision='rejected'`,
		"high-risk":                            `UPDATE admissions SET risk='high'`,
		"changed-policy-version":               `UPDATE admissions SET policy_version='other'`,
		"removed-gates":                        `UPDATE admissions SET required_gates='[]'`,
		"different-admission-correlation":      `UPDATE admissions SET correlation_id='other'`,
		"changed-admission-payload":            `UPDATE admission_heads SET payload=convert_to('{}','UTF8')`,
		"changed-plan-payload":                 `UPDATE planning_heads SET payload=convert_to('{}','UTF8')`,
	}
	for name, mutation := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			if _, err := f.accept(f.envelope(t, f.command)); err != nil {
				t.Fatal(err)
			}
			if _, err := f.Admin.Pool.Exec(mutation); err != nil {
				t.Fatal(err)
			}
			if claim, err := Claim(context.Background(), f.db, f.readyPolicy, f.command.ID()); err == nil || claim.Token != "" {
				t.Fatal("changed proof authorized execution")
			}
			if f.Count(t, `SELECT count(*) FROM execution_requests WHERE state='queued'`) != 1 {
				t.Fatal("invalid proof advanced claim")
			}
		})
	}
}

func TestExecutionReadinessBindsCommandAndCurrentConfiguration(t *testing.T) {
	for _, which := range []string{"source", "admission", "issue", "configuration"} {
		t.Run(which, func(t *testing.T) {
			f := newFixture(t)
			switch which {
			case "source":
				f.command.SourceSHA = "dddddddddddddddddddddddddddddddddddddddd"
			case "admission":
				f.command.AdmissionID = "admission-issue-dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
			case "issue":
				f.command.IssueNumber = 4
			}
			if _, err := f.accept(f.envelope(t, f.command)); err != nil {
				t.Fatal(err)
			}
			if which == "configuration" {
				f.Config.TriggerLabel = "different-ready"
			}
			if _, err := Claim(context.Background(), f.db, f.readyPolicy, f.command.ID()); err == nil {
				t.Fatal("mismatched current binding accepted")
			}
		})
	}
}

func TestExecutionReadinessSupersededDuringClaimLock(t *testing.T) {
	f := newFixture(t)
	if _, err := f.accept(f.envelope(t, f.command)); err != nil {
		t.Fatal(err)
	}
	lock, err := f.Admin.Pool.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	if _, err = lock.Exec(`SELECT id FROM execution_requests WHERE id=$1 FOR UPDATE`, f.command.ID()); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		claim, err := Claim(context.Background(), f.db, f.readyPolicy, f.command.ID())
		if claim.Token != "" {
			err = nil
		}
		result <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var waiting int
		if err = f.Admin.Pool.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE usename='aisdlc_worker' AND wait_event_type='Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("claim never reached actual lock wait")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err = f.Admin.Pool.Exec(`UPDATE intake_heads SET generation=2`); err != nil {
		t.Fatal(err)
	}
	if err = lock.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = <-result; err == nil {
		t.Fatal("superseded work obtained capability")
	}
	if f.Count(t, `SELECT count(*) FROM execution_requests WHERE state='stopped'`) != 1 {
		t.Fatal("superseded claim not stopped")
	}
}
