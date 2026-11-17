"""Post-diagnostic history retention on an explicitly disposable database only."""
import json
import secrets
import uuid
from types import SimpleNamespace

from diagnostic_upgrade_data import Compatibility
import diagnostic_schema
from runtime_upgrade_backup import Restore
from tool_budget_upgrade import ToolBudgetUpgrade
import upgrade_backup_data as data


def exercise(database, provider, image, directory, catalog, plan_id, first_work, report, save, original_authority):
    """Called only after the published-migration fixture owns its PostgreSQL CID."""
    work = str(uuid.uuid4())
    database.sql(f"INSERT INTO work_items(plan_id,id,uuid,issue_numbers,acceptance_criteria,status) VALUES('{plan_id}','retained-2','{work}','[1]','[\"keep diagnostic history\"]','failed');")
    for index, work_id in enumerate((first_work, work)):
        request, digest = 'execution-' + secrets.token_hex(32), secrets.token_hex(32)
        claim, pod, config = str(uuid.uuid4()), str(uuid.uuid4()), str(uuid.uuid4())
        database.sql(f"INSERT INTO execution_requests(id,work_item_uuid,attempt,repository,authority_revision,payload_digest,payload,receipt,expires_at,state,stop_reason) VALUES('{request}','{work_id}',1,'fixture/app','{digest}','{digest}',decode('00','hex'),decode('00','hex'),clock_timestamp()+interval '1 hour','stopped','uncertain');"
            f"INSERT INTO execution_lifecycles(request_id,claim_token,claim_until,binding_digest,source_digest,source_bytes,namespace,pod_name,config_name,phase,config_uid,pod_uid) VALUES('{request}','{claim}',clock_timestamp()-interval '1 second','{digest}','{digest}',1,'ai-sdlc-executors','executor-{work_id}-1','assignment-{secrets.token_hex(16)}','start-ready','{config}','{pod}');")
        if index:
            database.sql(f"INSERT INTO execution_failures(request_id,pod_uid,container_id,exit_code) VALUES('{request}','{pod}','containerd://{digest}',22);")
    upgrade = object.__new__(ToolBudgetUpgrade)
    intake_key, intake_authority = 'a' * 64, 'b' * 64
    payload = json.dumps({'authorityRevision': intake_authority}).encode().hex()
    database.sql(f"INSERT INTO intake_revisions(repository,issue_number,revision,generation,config_digest,event_id,payload) VALUES('fixture/app',1,'fixture',1,'{intake_key}','fixture-intake',decode('{payload}','hex'));"
        f"INSERT INTO intake_heads(repository,issue_number,content_key,generation) VALUES('fixture/app',1,'{intake_key}',1);"
        f"INSERT INTO intake_cursors(repository,config_digest,watermark,next_poll) VALUES('fixture/app','{intake_key}','2026-01-01','2026-01-01');"
        f"INSERT INTO intake_observations(repository,issue_number,generation,config_digest,authority_revision,content_key,observed_at) VALUES('fixture/app',1,1,'{intake_key}','{intake_authority}','{intake_key}','2026-01-01');")
    upgrade.catalog = catalog
    upgrade.database = lambda: database
    upgrade.parent = SimpleNamespace(record={'sourceProof': original_authority})
    upgrade.record = {'sourceProof': upgrade.snapshot(), 'sourceRoleIdentities': Compatibility(database).roles(),
                      'completed': 'source', 'pause': None, 'backup': None, 'transition': None, 'recoveryPause': None}
    proof = upgrade.record['sourceProof']
    if proof['work_items']['rows'] != 2 or proof['execution_failures']['rows'] != 1:
        raise ValueError('Expected two failed fixture attempts and one diagnostic')
    database.sql("UPDATE intake_cursors SET watermark='2026-01-02',next_poll='2026-01-02';"
                 "UPDATE intake_observations SET observed_at='2026-01-02';")
    for table, column, original in (
        ('intake_cursors', 'config_digest', intake_key),
        ('intake_observations', 'authority_revision', intake_authority),
        ('intake_observations', 'content_key', intake_key),
    ):
        try:
            database.sql(f"UPDATE {table} SET {column}='{'f' * 64}';")
            try:
                upgrade.capture_first_pause(database)
            except ValueError as error:
                if str(error) != 'Polling identities differ from original retained issue history':
                    raise
            else:
                raise ValueError('Foreign polling identity was accepted')
        finally:
            database.sql(f"UPDATE {table} SET {column}='{original}';")
    captured = upgrade.capture_first_pause(database)
    if captured == proof or upgrade.record['sourceProof'] != proof:
        raise ValueError('Polling must change only the new capture, not the preliminary evidence')
    upgrade.record['fencedSourceProof'] = captured
    proof = captured
    try:
        database.sql("UPDATE intake_observations SET observed_at='2026-01-03';")
        try:
            upgrade.retained()
        except ValueError:
            pass
        else:
            raise ValueError('Polling drift was allowed after the complete capture')
    finally:
        database.sql("UPDATE intake_observations SET observed_at='2026-01-02';")
    upgrade.retained()
    artifacts, captured = database.export()
    if captured != proof:
        raise ValueError('Post-diagnostic export changed retained history')
    directory.mkdir(mode=0o700)
    backup = {'archives': data.write_archives(directory, artifacts), 'retainedData': proof}
    def persist(record):
        report['toolBudgetHistoryRestore'] = record.get('restore')
        save()
    restore = Restore(provider, image, directory, backup, persist)
    try:
        restore.start()
        restore.execute()
    finally:
        restore.close()
    upgrade.retained()
    diagnostic_schema.verify(database)
    for mutation in (
        'ALTER TABLE execution_failures DISABLE ROW LEVEL SECURITY;',
        'ALTER TABLE execution_failures NO FORCE ROW LEVEL SECURITY;',
        'ALTER POLICY execution_failure_worker ON execution_failures USING (true) WITH CHECK (true);',
        'ALTER TABLE execution_failures DROP CONSTRAINT execution_failures_exit_code_check;',
        'ALTER TABLE execution_failures DISABLE TRIGGER ALL;',
        'CREATE RULE unexpected_insert AS ON INSERT TO execution_failures DO ALSO NOTIFY unexpected_diagnostic;',
        'CREATE TABLE unexpected_diagnostic_child () INHERITS (execution_failures);',
        'ALTER TABLE execution_failures SET UNLOGGED;',
        'GRANT TRUNCATE ON execution_failures TO aisdlc_worker;',
        'GRANT TRIGGER ON execution_failures TO aisdlc_worker;',
        'GRANT INSERT(observed_at) ON execution_failures TO aisdlc_worker;',
        'GRANT SELECT ON execution_failures TO aisdlc_worker WITH GRANT OPTION;',
        'REVOKE USAGE ON TYPE execution_failures FROM PUBLIC;',
        'ALTER TABLE execution_failures ALTER COLUMN exit_code ADD GENERATED ALWAYS AS IDENTITY;',
    ):
        # Execute DDL outside the rejection oracle: a failed mutation is not
        # evidence that the descriptor rejected a successfully changed table.
        observed = database.sql('BEGIN;' + mutation + diagnostic_schema.QUERY + 'ROLLBACK;')
        if json.loads(observed) == diagnostic_schema.EXPECTED:
            raise ValueError('Diagnostic mutation did not change the descriptor')
        class TransactionalControl:
            def sql(self, statement):
                if statement != diagnostic_schema.QUERY:
                    raise AssertionError('Unexpected diagnostic descriptor query')
                return observed
        try:
            diagnostic_schema.verify(TransactionalControl())
        except ValueError:
            pass
        else:
            raise ValueError('Changed diagnostic schema or row security was accepted')
        diagnostic_schema.verify(database)
    upgrade.retained()
    try:
        database.sql('UPDATE execution_failures SET exit_code=23;')
        try:
            upgrade.retained()
        except ValueError:
            pass
        else:
            raise ValueError('Changed diagnostic was accepted as original history')
    finally:
        database.sql('UPDATE execution_failures SET exit_code=22;')
    upgrade.retained()
    upgrade.original_database_authority()
    for field, mutation, restore_sql in (
        ('@membership', 'GRANT aisdlc_verifier TO aisdlc_worker;',
         'REVOKE aisdlc_verifier FROM aisdlc_worker;'),
        ('@role-settings', "ALTER ROLE aisdlc_worker SET statement_timeout='7s';",
         'ALTER ROLE aisdlc_worker RESET statement_timeout;'),
    ):
        try:
            database.sql(mutation)
            changed = upgrade.snapshot()
            if changed[field] == original_authority['data'][field]:
                raise ValueError('Authority mutation did not change the expected field')
            try:
                upgrade.original_database_authority(changed)
            except ValueError:
                pass
            else:
                raise ValueError('Changed original membership or settings were adopted')
        finally:
            database.sql(restore_sql)
        upgrade.original_database_authority()
        upgrade.retained()
    report.update(toolBudgetFailedAttemptsRetained=True, toolBudgetDiagnosticRetained=True,
        toolBudgetHistoryRestorationVerified=True, changedDiagnosticRejected=True,
        changedDiagnosticSchemaAndRLSRejected=True,
        changedDiagnosticRulesAndInheritanceRejected=True,
        changedDiagnosticDurabilityRejected=True,
        changedDiagnosticACLsRejected=True,
        changedOriginalMembershipAndRoleSettingsRejected=True,
        prePausePollingCaptured=True, postCapturePollingDriftRejected=True,
        changedPollingIdentitiesRejected=True,
        diagnosticTableEmpty=False,
        toolBudgetFixtureScope='isolated PostgreSQL retention/restore; no installed CLI or E2E proof')
    save()
