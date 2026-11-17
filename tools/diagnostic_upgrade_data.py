"""Retained-state proof for the reviewed additive diagnostic migration only."""
import hashlib
import json

import install


def digest(raw):
    return hashlib.sha256(raw if isinstance(raw, bytes) else raw.encode()).hexdigest()


class Compatibility:
    def __init__(self, database):
        self.database = database

    def schema(self):
        db = self.database
        return digest(db.command(db.prefix + ['pg_dump', '--schema-only', '--create', '--no-password',
            '--restrict-key=aiSdlcSchemaComparisonOnly', '--no-privileges',
            '--exclude-table=public.execution_failures', '-U', db.user, '-d', 'ai_sdlc']))

    def permissions(self, before=''):
        # before is used only by the disposable negative-control harness. Its
        # transaction is always rolled back and is never a workflow operation.
        return self.database.sql('BEGIN;' + before + ACL_QUERY + 'ROLLBACK;')

    def roles(self):
        return digest(self.database.sql("SELECT to_jsonb(r)-'rolpassword' FROM pg_authid r ORDER BY rolname;"))

    def tracking(self):
        return digest(self.database.sql("SELECT row_to_json(t)::text FROM (SELECT * FROM schema_migrations WHERE filename<>'0034_execution_failures.sql' ORDER BY filename) t;"))

    def observe(self):
        if self.database.sql("SELECT count(*) FROM pg_namespace WHERE nspname NOT IN('public','pg_catalog','information_schema') AND nspname !~ '^pg_(toast|temp)';") != '0\n':
            raise ValueError('Unexpected application schema outside reviewed scope')
        return {'data': self.database.snapshot(), 'schema': self.schema(), 'roles': self.roles(),
                'permissions': self.permissions(), 'tracking': self.tracking()}

    def source(self, catalog):
        self.quiet()
        proof = self.observe()
        names = json.loads(self.database.sql('SELECT json_agg(filename ORDER BY filename) FROM schema_migrations;'))
        if (len(catalog) != 33 or names != [v['filename'] for v in catalog]
                or proof['data']['schema_migrations']['rows'] != 33 or 'execution_failures' in proof['data']):
            raise ValueError('Original exact 33-migration source required')
        return proof

    def verify(self, source, *, allow_unmigrated=False):
        self.quiet()
        current = self.observe()
        old, new = source['data'], current['data']
        migrated = 'execution_failures' in new
        if not migrated and not allow_unmigrated:
            raise ValueError('Diagnostic migration has not completed')
        if set(new) != set(old) | ({'execution_failures'} if migrated else set()):
            raise ValueError('Unexpected post-migration table inventory')
        if any(old[key] != new[key] for key in old if key not in ('@roles', '@schema', 'schema_migrations')):
            raise ValueError('Original retained history or role membership changed')
        if (new['schema_migrations']['rows'] != (34 if migrated else 33)
                or migrated and new['execution_failures']['rows'] != 0
                or any(source[key] != current[key] for key in ('schema', 'roles', 'permissions', 'tracking'))):
            raise ValueError('Original schema, ACLs, identities or migration history changed')
        if migrated and not allow_unmigrated:
            self.diagnostic_privileges()
        return {'catalogSize': 34 if migrated else 33, 'originalRowsUnchanged': True,
                'originalSchemaAndPrivilegesUnchanged': True, 'originalRolesUnchanged': True,
                'originalTrackingUnchanged': True, 'workflowEvidence': False}

    def quiet(self):
        # This bridge only resumes contained failed history. The unsigned
        # legacy outbox is not a runtime delivery queue and cannot be promoted.
        result = self.database.sql("SELECT NOT (EXISTS(SELECT 1 FROM work_items WHERE status<>'failed') OR EXISTS(SELECT 1 FROM execution_requests WHERE state<>'stopped') OR EXISTS(SELECT 1 FROM authorized_outbox WHERE published_at IS NULL AND NOT exhausted) OR EXISTS(SELECT 1 FROM authorized_inbox WHERE result IS NULL));")
        if result != 't\n':
            raise ValueError('Queued work or incomplete authenticated delivery prevents diagnostic resume')

    def diagnostic_privileges(self):
        privileges = json.loads(self.database.sql("SELECT json_build_object('select',has_table_privilege('aisdlc_worker','execution_failures','SELECT'),'insert',has_column_privilege('aisdlc_worker','execution_failures','request_id','INSERT'),'update',has_table_privilege('aisdlc_worker','execution_failures','UPDATE'),'delete',has_table_privilege('aisdlc_worker','execution_failures','DELETE'),'timestamp',has_column_privilege('aisdlc_worker','execution_failures','observed_at','INSERT'));"))
        if privileges != {'select': True, 'insert': True, 'update': False, 'delete': False, 'timestamp': False}:
            raise ValueError('Diagnostic privileges differ from reviewed boundary')
        for component in install.ALL_ROLES:
            if component == 'worker':
                continue
            role = 'aisdlc_' + component.replace('-', '_')
            if self.database.sql(f"SELECT NOT (has_table_privilege('{role}','execution_failures','SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER') OR has_any_column_privilege('{role}','execution_failures','SELECT,INSERT,UPDATE,REFERENCES'));") != 't\n':
                raise ValueError('Unrelated component received diagnostic authority')
        if self.database.sql("SELECT NOT EXISTS(SELECT 1 FROM pg_class c CROSS JOIN LATERAL aclexplode(c.relacl) a WHERE c.oid='execution_failures'::regclass AND a.grantee=0);") != 't\n':
            raise ValueError('PUBLIC received diagnostic authority')


ACL_QUERY = """WITH objects AS (
SELECT 'schema' kind,n.nspname object,n.nspowner owner,n.nspacl acl,'n' objtype FROM pg_namespace n WHERE n.nspname='public'
UNION ALL SELECT 'relation',n.nspname||'.'||c.relname,c.relowner,c.relacl,CASE WHEN c.relkind='S' THEN 's' ELSE 'r' END FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relkind IN('r','p','v','m','S','f') AND c.relname<>'execution_failures'
UNION ALL SELECT 'column',n.nspname||'.'||c.relname||'.'||a.attname,c.relowner,coalesce(a.attacl,'{}'::aclitem[]),NULL FROM pg_attribute a JOIN pg_class c ON c.oid=a.attrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relkind IN('r','p','v','m','f') AND c.relname<>'execution_failures' AND a.attnum>0 AND NOT a.attisdropped
UNION ALL SELECT 'function',p.oid::regprocedure::text,p.proowner,p.proacl,'f' FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='public'
UNION ALL SELECT 'database',d.datname,d.datdba,d.datacl,'d' FROM pg_database d WHERE d.datname=current_database()
UNION ALL SELECT 'default',r.rolname||':'||coalesce(n.nspname,'@global')||':'||d.defaclobjtype::text,d.defaclrole,d.defaclacl,NULL FROM pg_default_acl d JOIN pg_roles r ON r.oid=d.defaclrole LEFT JOIN pg_namespace n ON n.oid=d.defaclnamespace
UNION ALL SELECT 'type',n.nspname||'.'||t.typname,t.typowner,t.typacl,'T' FROM pg_type t JOIN pg_namespace n ON n.oid=t.typnamespace WHERE n.nspname='public' AND t.oid NOT IN (SELECT c.reltype FROM pg_class c WHERE c.oid=to_regclass('public.execution_failures') UNION SELECT u.typarray FROM pg_class c JOIN pg_type u ON u.oid=c.reltype WHERE c.oid=to_regclass('public.execution_failures'))
UNION ALL SELECT 'tablespace',t.spcname,t.spcowner,t.spcacl,'t' FROM pg_tablespace t
UNION ALL SELECT 'language',l.lanname,l.lanowner,l.lanacl,'l' FROM pg_language l
UNION ALL SELECT 'foreign-data-wrapper',f.fdwname,f.fdwowner,f.fdwacl,'F' FROM pg_foreign_data_wrapper f
UNION ALL SELECT 'foreign-server',f.srvname,f.srvowner,f.srvacl,'S' FROM pg_foreign_server f
UNION ALL SELECT 'large-object',l.oid::text,l.lomowner,l.lomacl,'L' FROM pg_largeobject_metadata l
UNION ALL SELECT 'parameter',p.parname,NULL::oid,p.paracl,NULL FROM pg_parameter_acl p
), entries AS (SELECT o.kind,o.object,own.rolname owner,o.acl IS NULL default_acl,g.rolname grantor,coalesce(r.rolname,'PUBLIC') grantee,a.privilege_type,a.is_grantable FROM objects o LEFT JOIN pg_roles own ON own.oid=o.owner LEFT JOIN LATERAL aclexplode(CASE WHEN cardinality(o.acl)=0 THEN NULL::aclitem[] ELSE o.acl END) a ON true LEFT JOIN pg_roles g ON g.oid=a.grantor LEFT JOIN pg_roles r ON r.oid=a.grantee)
SELECT jsonb_build_object('entries',count(*),'sha256',encode(sha256(convert_to(coalesce(jsonb_agg(to_jsonb(e) ORDER BY to_jsonb(e)::text)::text,'[]'),'UTF8')),'hex')) FROM entries e;"""
