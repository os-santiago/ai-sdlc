"""Retain every schema-34 fact/privilege across the append-only browser bridge."""
import json

from diagnostic_upgrade_data import Compatibility as Previous, ACL_QUERY, digest
import reviewer_upgrade_history

TABLES=tuple('deployment_browser_'+x for x in ('cleanups','evidence','intents','pods','sends'))
MIGRATION='0035_deployment_browser.sql'
quoted=','.join("'"+x+"'" for x in TABLES)
# Use the established full authority inventory; exclude only the five new
# relation/column/type objects, never the existing execution diagnostic table.
ACL=ACL_QUERY.replace("c.relname<>'execution_failures'",'c.relname NOT IN('+quoted+')')
ACL=ACL.replace("c.oid=to_regclass('public.execution_failures')",'c.relname IN('+quoted+") AND c.relnamespace='public'::regnamespace")


class Compatibility(Previous):
    def quiet(self):reviewer_upgrade_history.quiet(self.database)

    def schema(self):
        db=self.database
        return digest(db.command(db.prefix+['pg_dump','--schema-only','--create','--no-password',
            '--restrict-key=aiSdlcSchemaComparisonOnly','--no-privileges',
            *['--exclude-table=public.'+x for x in TABLES],'-U',db.user,'-d','ai_sdlc']))

    def permissions(self,before=''):
        return self.database.sql('BEGIN;'+before+ACL+'ROLLBACK;')

    def tracking(self):
        return digest(self.database.sql("SELECT row_to_json(t)::text FROM (SELECT * FROM schema_migrations WHERE filename<>'"+MIGRATION+"' ORDER BY filename) t;"))

    def source(self,catalog):
        self.quiet();proof=self.observe()
        names=json.loads(self.database.sql('SELECT json_agg(filename ORDER BY filename) FROM schema_migrations;'))
        if len(catalog)!=34 or names!=[x['filename'] for x in catalog] or set(TABLES)&set(proof['data']):
            raise ValueError('Original exact schema-34 browser source required')
        return proof

    def verify(self,source,catalog):
        self.quiet();current=self.observe();old,new=source['data'],current['data']
        names=json.loads(self.database.sql('SELECT json_agg(filename ORDER BY filename) FROM schema_migrations;'))
        if len(catalog)!=35 or names!=[x['filename'] for x in catalog] or names[-1]!=MIGRATION:
            raise ValueError('Exact target browser migration catalog required')
        if set(new)!=set(old)|set(TABLES) or any(new[x]['rows']!=0 for x in TABLES):
            raise ValueError('Browser migration inventory or empty history differs')
        if any(old[k]!=new[k] for k in old if k not in ('@roles','@schema','schema_migrations')):
            raise ValueError('Original workflow rows or database authority changed')
        if any(source[k]!=current[k] for k in ('schema','roles','permissions','tracking')):
            raise ValueError('Original schema, privileges, role identities or tracking changed')
        # This separately reviewed constant comes from a disposable PostgreSQL
        # migration fixture, never from the installation being upgraded.
        import browser_schema
        browser_schema.verify(self.database)
        return {'catalogSize':35,'originalRowsUnchanged':True,'originalSchemaAndPrivilegesUnchanged':True,
                'originalRolesUnchanged':True,'originalTrackingUnchanged':True,'browserTablesEmpty':True,
                'workflowEvidence':False}
