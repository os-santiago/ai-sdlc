"""Real Git object tests; publication validation uses the existing resolver."""
import contextlib
import io
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import install
import install_upgrade as upgrade


class UpgradePlanTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.git('init', '-q')
        self.git('config', 'user.name', 'Fixture')
        self.git('config', 'user.email', 'fixture@example.invalid')
        self.files = self.root / upgrade.MIGRATIONS
        self.files.mkdir(parents=True)
        self.write('0001_initial.sql', 'CREATE TABLE history(id integer);\n')
        self.before = self.commit()
        self.write('0002_additive.sql', 'CREATE TABLE extra(id integer);\n')
        self.after = self.commit()
        self.source = upgrade.Source(self.root)

    def git(self, *args):
        env = {key: value for key, value in os.environ.items() if not key.startswith('GIT_')}
        return subprocess.check_output(['git', '-C', str(self.root), *args], stderr=subprocess.DEVNULL, env=env).decode().strip()

    def write(self, name, content):
        (self.files/name).write_text(content)

    def commit(self):
        self.git('add', '.')
        self.git('commit', '-qm', 'fixture')
        return self.git('rev-parse', 'HEAD')

    def resolve(self, api, run, revision, **kwargs):
        # Every planned publication is signature-checked for its own revision.
        self.assertTrue(callable(kwargs.pop('verify')))
        self.assertEqual(kwargs, {'include_build': True, 'include_release': True})
        return {}, {'runURL': 'https://github.com/os-santiago/ai-sdlc/actions/runs/'+str(run),
                    'images': {'migrator': {'digest': 'sha256:'+('1' if revision == self.before else '2')*64}}}

    def plan(self, source=None, target=None):
        with patch.object(upgrade.install_images, 'resolve', side_effect=self.resolve):
            return upgrade.plan(self.source, object(), 1, source or self.before, 2, target or self.after)

    def test_exact_git_objects_ignore_dirty_worktree_and_replacement(self):
        old = self.source.catalog(self.before)
        new = self.source.catalog(self.after)
        # Both tracked dirty content and Git replacement objects must be ignored.
        self.write('0001_initial.sql', 'DROP TABLE history;\n')
        self.git('replace', self.before, self.after)
        self.assertEqual(self.source.catalog(self.before), old)
        self.assertEqual(self.source.catalog(self.after), new)
        result = self.plan()
        self.assertEqual(result['pendingMigrations'], new[1:])
        self.assertEqual(result['changedComponents'], ['migrator'])
        self.assertFalse(result['upgradeAuthorized'])
        self.assertFalse(result['rollbackAuthorized'])
        self.assertFalse(result['historicalSQLHashesVerified'])
        self.assertIn('runtime-owned-drain-and-containment', result['unverifiedGates'])
        self.assertIn('database-schema-integrity', result['unverifiedGates'])
        self.assertEqual(self.git('status', '--porcelain'), 'M runtime/k8s/internal/store/migrations/0001_initial.sql')

    def test_unchanged_sql_does_not_claim_upgrade_or_database_state(self):
        result = self.plan(self.after, self.after)
        self.assertEqual(result['pendingMigrations'], [])
        self.assertEqual(result['changedComponents'], [])
        self.assertFalse(result['upgradeAuthorized'])
        self.assertFalse(result['workflowEvidence'])
        self.assertIn('installed-source-identity', result['unverifiedGates'])

    def test_inherited_git_locations_cannot_read_or_modify_foreign_repository(self):
        foreign_tmp = tempfile.TemporaryDirectory()
        self.addCleanup(foreign_tmp.cleanup)
        foreign = Path(foreign_tmp.name)
        original_root = self.root
        try:
            self.root = foreign
            self.git('init', '-q')
            self.git('config', 'user.name', 'Foreign fixture')
            self.git('config', 'user.email', 'foreign@example.invalid')
            (foreign/'sentinel').write_text('must remain unchanged\n')
            foreign_head = self.commit()
            index_before = (foreign/'.git/index').read_bytes()
            config_before = (foreign/'.git/config').read_bytes()
        finally:
            self.root = original_root
        expected = self.source.catalog(self.before)
        hostile = {'GIT_DIR': str(foreign/'.git'), 'GIT_COMMON_DIR': str(foreign/'.git'),
                   'GIT_WORK_TREE': str(foreign), 'GIT_INDEX_FILE': str(foreign/'.git/index'),
                   'GIT_OBJECT_DIRECTORY': str(foreign/'.git/objects'),
                   'GIT_ALTERNATE_OBJECT_DIRECTORIES': str(foreign/'.git/objects')}
        with patch.dict(os.environ, hostile):
            self.assertEqual(self.source.catalog(self.before), expected)
            self.git('init', '-q')
            self.write('0003_local.sql', 'SELECT 1;\n')
            self.commit()
        self.assertEqual((foreign/'.git/index').read_bytes(), index_before)
        self.assertEqual((foreign/'.git/config').read_bytes(), config_before)
        try:
            self.root = foreign
            self.assertEqual(self.git('rev-parse', 'HEAD'), foreign_head)
            self.assertEqual(self.git('status', '--porcelain'), '')
        finally:
            self.root = original_root

    def test_reject_changed_removed_or_inserted_prefix(self):
        before = self.source.catalog(self.before)
        after = self.source.catalog(self.after)
        variants = [[], after[1:], [dict(after[0], sha256='0'*64), after[1]],
                    [{'filename': '0000_inserted.sql', 'sha256':'0'*64}]+after]
        for target in variants:
            with self.subTest(target=target), self.assertRaises(ValueError):
                upgrade.pending_migrations(before, target)
        self.write('0001_initial.sql', 'DROP TABLE history;\n')
        changed = self.commit()
        with self.assertRaises(ValueError):
            self.plan(target=changed)

    def test_reject_downgrade_and_noncommit(self):
        with self.assertRaises(ValueError):
            self.plan(self.after, self.before)
        blob = self.git('rev-parse', self.before+':'+upgrade.MIGRATIONS+'0001_initial.sql')
        with self.assertRaises(ValueError):
            self.source.catalog(blob)
        with self.assertRaises(ValueError):
            self.source.catalog('main')

    def test_legacy_grafts_cannot_fabricate_published_ancestry(self):
        tree = self.git('rev-parse', self.after+'^{tree}')
        unrelated = self.git('commit-tree', tree, '-m', 'unrelated source history')
        self.assertEqual(self.source.catalog(unrelated), self.source.catalog(self.after))
        with self.assertRaises(ValueError):
            self.plan(unrelated, self.after)
        # A legacy graft changes parent traversal even with replace refs off.
        # The exact source commit remains unrelated and must still be rejected.
        (self.root/'.git/info/grafts').write_text(self.after+' '+unrelated+'\n')
        with self.assertRaises(ValueError):
            self.plan(unrelated, self.after)

    def test_reject_symlink_and_noncanonical_paths(self):
        (self.files/'0003_link.sql').symlink_to('0001_initial.sql')
        with self.assertRaises(ValueError):
            self.source.catalog(self.commit())
        (self.files/'0003_link.sql').unlink()
        (self.files/'nested').mkdir()
        (self.files/'nested'/'0003_nested.sql').write_text('SELECT 1;')
        with self.assertRaises(ValueError):
            self.source.catalog(self.commit())

    def test_reject_empty_oversized_and_executable_sql(self):
        for content in ('', 'x'*(256*1024+1)):
            self.write('0003_invalid.sql', content)
            with self.assertRaises(ValueError):
                self.source.catalog(self.commit())
        self.write('0003_invalid.sql', 'SELECT 1;')
        (self.files/'0003_invalid.sql').chmod(0o755)
        with self.assertRaises(ValueError):
            self.source.catalog(self.commit())

    def test_publication_rejection_cannot_emit_plan(self):
        with patch.object(upgrade.install_images, 'resolve', side_effect=ValueError('not approved')):
            with self.assertRaises(ValueError):
                upgrade.plan(self.source, object(), 1, self.before, 2, self.after)
        with patch.object(upgrade.install_images, 'resolve') as resolver:
            for run in (0, -1, True, 2**63):
                with self.assertRaises(ValueError):
                    upgrade.plan(self.source, object(), run, self.before, 2, self.after)
            resolver.assert_not_called()

    def test_publication_binding_and_single_entrypoint(self):
        with patch.object(upgrade.install_images, 'resolve', side_effect=self.resolve) as resolver:
            upgrade.plan(self.source, 'api', 1, self.before, 2, self.after)
            self.assertEqual([c.args for c in resolver.call_args_list],
                             [('api', 1, self.before), ('api', 2, self.after)])
        args = ['--source-run', '1', '--source-revision', self.before,
                '--target-run', '2', '--target-revision', self.after]
        with patch.object(upgrade, 'Source', return_value=self.source), \
             patch.object(upgrade.install_images, 'resolve', side_effect=self.resolve), \
             patch.object(install, 'tools', side_effect=AssertionError('no cluster tools')), \
             contextlib.redirect_stdout(io.StringIO()) as output:
            self.assertEqual(install.main(['upgrade-plan', *args]), 0)
        self.assertFalse(json.loads(output.getvalue())['upgradeAuthorized'])


if __name__ == '__main__':
    unittest.main()
