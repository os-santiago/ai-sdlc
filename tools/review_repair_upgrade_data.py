"""Compare a schema37 installation with the same migration rehearsed on backup."""
import json

from diagnostic_upgrade_data import ACL_QUERY, Compatibility as Previous, digest
import review_feedback_upgrade_data as feedback

MIGRATION = '0037_review_repair.sql'
TABLES = ('execution_repairs', 'repair_model_budgets', 'review_repair_handoffs', 'review_repair_stops')
# Canonical, order-independent privileges of every database object. Languages and
# tablespaces are cluster bootstrap objects owned by each server's initdb
# superuser (the restore administrator in the rehearsal); migrations never touch
# them. ACL array order is historical, so a full dump is not order-stable.
ACL = ACL_QUERY.replace('execution_failures', 'ai_sdlc_no_relation').replace(
    'FROM entries e;', "FROM entries e WHERE e.kind NOT IN ('language','tablespace');")


class Compatibility(Previous):
    def quiet(self):
        feedback.Compatibility(self.database).quiet()
        if self.database.sql('SELECT count(*) FROM review_feedback;') != '0\n':
            raise ValueError('Repair installation cannot adopt historical feedback')
        names = json.loads(self.database.sql("SELECT json_agg(tablename ORDER BY tablename) FROM pg_tables WHERE schemaname='public';"))
        present = set(TABLES) & set(names)
        if present and present != set(TABLES):
            raise ValueError('Partial repair schema cannot be adopted')
        for table in present:
            if self.database.sql('SELECT count(*) FROM '+table+';') != '0\n':
                raise ValueError('Existing repair history cannot be adopted')

    def ddl(self):
        db = self.database
        return digest(db.command(db.prefix + ['pg_dump', '--schema-only', '--create', '--no-password',
            '--restrict-key=aiSdlcSchemaComparisonOnly', '--no-privileges', '-U', db.user, '-d', 'ai_sdlc']))

    def privileges(self):
        return self.database.sql('BEGIN;' + ACL + 'ROLLBACK;')

    def tracking(self):
        return digest(self.database.sql("SELECT row_to_json(t)::text FROM (SELECT * FROM schema_migrations WHERE filename<>'"+MIGRATION+"' ORDER BY filename) t;"))

    def canonical_roles(self, exclude_restore_role=None):
        clause = ''
        if exclude_restore_role is not None:
            import re
            if not re.fullmatch('aisdlc_restore_[a-f0-9]{32}', exclude_restore_role):
                raise ValueError('Only the owned restore administrator may be excluded')
            clause = " WHERE rolname<>'"+exclude_restore_role+"'"
        # Reprovisioning retains passwords but PostgreSQL chooses fresh SCRAM
        # salts. Compare all other attributes; the installer separately proves
        # original role OIDs and successful authentication with original secrets.
        return digest(self.database.sql("SELECT to_jsonb(r)-'oid'-'rolpassword' FROM pg_authid r"+clause+" ORDER BY rolname;"))

    def catalog(self, catalog, count):
        names = json.loads(self.database.sql('SELECT json_agg(filename ORDER BY filename) FROM schema_migrations;'))
        if len(catalog) != count or names != [v['filename'] for v in catalog] or (count == 37 and names[-1] != MIGRATION):
            raise ValueError('Exact repair bridge migration catalog required')

    def source(self, catalog):
        self.quiet()
        self.catalog(catalog,36)
        proof = self.database.snapshot()
        if set(TABLES) & set(proof):
            raise ValueError('Original schema36 source required')
        return dict(data=proof, roles=self.roles(), canonicalRoles=self.canonical_roles(), tracking=self.tracking())

    def rehearsed(self, source, catalog, restore_role):
        self.quiet()
        self.catalog(catalog,37)
        current = self.database.snapshot(exclude_restore_role=restore_role)
        self.history(source, current)
        if self.canonical_roles(restore_role) != source['canonicalRoles']:
            raise ValueError('Rehearsal changed role attributes')
        return dict(schema=self.ddl(), permissions=self.privileges(), canonicalRoles=self.canonical_roles(restore_role),
                    catalogSize=37, historyPreserved=True)

    def history(self, source, current):
        old=source['data']
        if set(current) != set(old)|set(TABLES):
            raise ValueError('Repair migration inventory changed')
        if any(current[k] != old[k] for k in old if k not in ('@schema','@roles','schema_migrations')):
            raise ValueError('Original workflow data, membership or role settings changed')
        if any(current[t]['rows'] != 0 for t in TABLES) or current['review_feedback']['rows'] != 0:
            raise ValueError('Repair installation created workflow history')
        if self.tracking() != source['tracking']:
            raise ValueError('Original migration tracking changed')

    def verify(self, source, expected, catalog):
        self.quiet()
        self.catalog(catalog,37)
        current=self.database.snapshot()
        self.history(source,current)
        if (set(expected) != {'schema','permissions','canonicalRoles','catalogSize','historyPreserved'}
                or expected['catalogSize'] != 37 or expected['historyPreserved'] is not True
                or self.ddl() != expected['schema'] or self.privileges() != expected['permissions']
                or self.canonical_roles() != expected['canonicalRoles']
                or self.roles() != source['roles']):
            raise ValueError('Repair DDL, RLS, ACL, function or original role identity differs from rehearsal')
        return dict(catalogSize=37, originalRowsUnchanged=True, originalRoleIdentitiesUnchanged=True,
                    originalTrackingUnchanged=True, exactRehearsedSchemaAndPrivileges=True,
                    feedbackAndRepairHistoryEmpty=True, workflowEvidence=False)
