"""Closed PostgreSQL metadata for reviewed migration 0034_execution_failures."""
import json

# Bound to the pinned PostgreSQL release and the reviewed additive DDL, not a
# fingerprint accepted from the installation that is about to be changed.
EXPECTED = {
    'owner': 'ai_sdlc', 'kind': 'r', 'rls': True, 'forceRls': True,
    'columns': [['request_id', 'text', True, None], ['pod_uid', 'uuid', True, None],
        ['container_id', 'text', True, None], ['exit_code', 'integer', True, None],
        ['observed_at', 'timestamp with time zone', True, 'clock_timestamp()']],
    'constraints': [
        ['c', 'CHECK (((exit_code >= 1) AND (exit_code <= 255)))', True, False],
        ['c', "CHECK ((container_id ~ '^[a-z0-9-]{1,32}://[a-f0-9]{32,128}$'::text))", True, False],
        ['f', 'FOREIGN KEY (request_id) REFERENCES execution_lifecycles(request_id)', True, False],
        ['p', 'PRIMARY KEY (request_id)', True, False]],
    'policies': [['execution_failure_worker', '*', True, ['0'],
        "(CURRENT_USER = 'aisdlc_worker'::name)", "(CURRENT_USER = 'aisdlc_worker'::name)"]],
    'userTriggers': 0, 'internalTriggers': 2, 'disabledTriggers': 0, 'droppedColumns': 0,
    'rules': 0, 'inheritanceLinks': 0,
    'indexes': [[True, True, True, True, True]],
}

QUERY = """SELECT json_build_object(
'owner',pg_get_userbyid(c.relowner),'kind',c.relkind,'rls',c.relrowsecurity,'forceRls',c.relforcerowsecurity,
'columns',(SELECT json_agg(json_build_array(a.attname,format_type(a.atttypid,a.atttypmod),a.attnotnull,pg_get_expr(d.adbin,d.adrelid)) ORDER BY a.attnum) FROM pg_attribute a LEFT JOIN pg_attrdef d ON d.adrelid=a.attrelid AND d.adnum=a.attnum WHERE a.attrelid=c.oid AND a.attnum>0 AND NOT a.attisdropped),
'constraints',(SELECT json_agg(json_build_array(con.contype,pg_get_constraintdef(con.oid),con.convalidated,con.condeferrable) ORDER BY con.contype,pg_get_constraintdef(con.oid) COLLATE "C") FROM pg_constraint con WHERE con.conrelid=c.oid),
'policies',(SELECT json_agg(json_build_array(pol.polname,pol.polcmd,pol.polpermissive,pol.polroles,pg_get_expr(pol.polqual,pol.polrelid),pg_get_expr(pol.polwithcheck,pol.polrelid)) ORDER BY pol.polname COLLATE "C") FROM pg_policy pol WHERE pol.polrelid=c.oid),
'userTriggers',(SELECT count(*) FROM pg_trigger t WHERE t.tgrelid=c.oid AND NOT t.tgisinternal),
'internalTriggers',(SELECT count(*) FROM pg_trigger t WHERE t.tgrelid=c.oid AND t.tgisinternal),
'disabledTriggers',(SELECT count(*) FROM pg_trigger t WHERE t.tgrelid=c.oid AND t.tgenabled<>'O'),
'droppedColumns',(SELECT count(*) FROM pg_attribute a WHERE a.attrelid=c.oid AND a.attnum>0 AND a.attisdropped),
'rules',(SELECT count(*) FROM pg_rewrite r WHERE r.ev_class=c.oid),
'inheritanceLinks',(SELECT count(*) FROM pg_inherits i WHERE i.inhrelid=c.oid OR i.inhparent=c.oid),
'indexes',(SELECT json_agg(json_build_array(i.indisvalid,i.indisready,i.indislive,i.indisunique,i.indisprimary) ORDER BY i.indexrelid) FROM pg_index i WHERE i.indrelid=c.oid)
) FROM pg_class c WHERE c.oid=to_regclass('public.execution_failures');"""


def verify(database):
    if json.loads(database.sql(QUERY)) != EXPECTED:
        raise ValueError('Original diagnostic table structure or row security changed')
