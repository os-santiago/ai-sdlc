"""Post-diagnostic history retention on an explicitly disposable database only."""
import secrets
import uuid

from diagnostic_upgrade_data import Compatibility
from runtime_upgrade_backup import Restore
from tool_budget_upgrade import ToolBudgetUpgrade
import upgrade_backup_data as data


def exercise(database, provider, image, directory, catalog, plan_id, first_work, report, save):
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
    upgrade.catalog = catalog
    upgrade.database = lambda: database
    upgrade.record = {'sourceProof': upgrade.snapshot(), 'sourceRoleIdentities': Compatibility(database).roles()}
    proof = upgrade.record['sourceProof']
    if proof['work_items']['rows'] != 2 or proof['execution_failures']['rows'] != 1:
        raise ValueError('Expected two failed fixture attempts and one diagnostic')
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
    report.update(toolBudgetFailedAttemptsRetained=True, toolBudgetDiagnosticRetained=True,
        toolBudgetHistoryRestorationVerified=True, changedDiagnosticRejected=True,
        diagnosticTableEmpty=False,
        toolBudgetFixtureScope='isolated PostgreSQL retention/restore; no installed CLI or E2E proof')
    save()
