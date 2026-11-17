"""Preserve schema35 history/authority while adding private review feedback."""
import json

from diagnostic_upgrade_data import Compatibility as Previous, ACL_QUERY, digest
import reviewer_upgrade_history
import review_feedback_schema

TABLES = ('review_feedback',)
MIGRATION = '0036_review_feedback.sql'
ACL = ACL_QUERY.replace('execution_failures', 'review_feedback')


class Compatibility(Previous):
    def quiet(self):
        reviewer_upgrade_history.quiet(self.database)
        for name in ('cleanups', 'evidence', 'intents', 'pods', 'sends'):
            if self.database.sql('SELECT count(*) FROM deployment_browser_' + name + ';') != '0\n':
                raise ValueError('Browser history prevents feedback maintenance')

    def schema(self):
        db = self.database
        return digest(db.command(db.prefix + ['pg_dump', '--schema-only', '--create', '--no-password',
            '--restrict-key=aiSdlcSchemaComparisonOnly', '--no-privileges',
            '--exclude-table=public.review_feedback', '-U', db.user, '-d', 'ai_sdlc']))

    def permissions(self, before=''):
        return self.database.sql('BEGIN;' + before + ACL + 'ROLLBACK;')

    def tracking(self):
        return digest(self.database.sql("SELECT row_to_json(t)::text FROM (SELECT * FROM schema_migrations WHERE filename<>'" + MIGRATION + "' ORDER BY filename) t;"))

    def source(self, catalog):
        self.quiet()
        proof = self.observe()
        names = json.loads(self.database.sql('SELECT json_agg(filename ORDER BY filename) FROM schema_migrations;'))
        if len(catalog) != 35 or names != [x['filename'] for x in catalog] or set(TABLES) & set(proof['data']):
            raise ValueError('Original exact schema35 feedback source required')
        return proof

    def verify(self, source, catalog):
        self.quiet()
        current = self.observe()
        old, new = source['data'], current['data']
        names = json.loads(self.database.sql('SELECT json_agg(filename ORDER BY filename) FROM schema_migrations;'))
        if len(catalog) != 36 or names != [x['filename'] for x in catalog] or names[-1] != MIGRATION:
            raise ValueError('Exact target feedback migration catalog required')
        if set(new) != set(old) | set(TABLES) or new['review_feedback']['rows'] != 0:
            raise ValueError('Feedback migration inventory or empty history differs')
        if any(old[k] != new[k] for k in old if k not in ('@roles', '@schema', 'schema_migrations')):
            raise ValueError('Original workflow rows or database authority changed')
        if any(source[k] != current[k] for k in ('schema', 'roles', 'permissions', 'tracking')):
            raise ValueError('Original schema, privileges, role identities or tracking changed')
        review_feedback_schema.verify(self.database)
        return {'catalogSize': 36, 'originalRowsUnchanged': True,
                'originalSchemaAndPrivilegesUnchanged': True, 'originalRolesUnchanged': True,
                'originalTrackingUnchanged': True, 'feedbackTableEmpty': True, 'workflowEvidence': False}
