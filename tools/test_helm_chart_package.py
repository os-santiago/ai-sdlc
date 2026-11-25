"""The packaged chart: deterministic version, lint, rendering, publication workflow."""
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import unittest

import yaml

import chart_release
from test_helm import CHART, HELM, LOCAL

ROOT = Path(__file__).resolve().parents[1]
WORKFLOW = ROOT / '.github/workflows/chart.yml'
LOCK = ROOT / 'tools/install-tools.lock.json'
GUIDE = ROOT / 'docs/operations/helm-chart-install.md'
REVISION = '0123456789abcdef0123456789abcdef01234567'
IDENTITY = 'https://github.com/os-santiago/ai-sdlc/.github/workflows/chart.yml@refs/heads/main'


class VersionTests(unittest.TestCase):
    def test_published_version_is_the_chart_version_plus_the_revision(self):
        chart = (CHART / 'Chart.yaml').read_text()
        version = chart_release.committed_version(chart)
        self.assertEqual(chart_release.published_version(chart, REVISION), version + '+0123456789ab')
        self.assertEqual(chart_release.oci_tag(version + '+0123456789ab'), version + '_0123456789ab')

    def test_version_inputs_are_strict(self):
        for revision in ('', 'HEAD', REVISION[:12], REVISION.upper(), REVISION + '0'):
            with self.assertRaises(ValueError):
                chart_release.published_version('version: 1.2.3\n', revision)
        for chart in ('', 'version: 1.2\n', 'version: 1.2.3-rc.1\n', 'version: 1.2.3+meta\n',
                      'version: 01.2.3\n', 'version: 1.2.3\nversion: 1.2.4\n'):
            with self.assertRaises(ValueError):
                chart_release.committed_version(chart)
        self.assertEqual(chart_release.committed_version('name: x\nversion: "1.2.3"  # pinned\n'), '1.2.3')

    def test_label_stays_within_the_kubernetes_limit(self):
        version = chart_release.published_version((CHART / 'Chart.yaml').read_text(), REVISION)
        label = (chart_release.CHART_NAME + '-' + version).replace('+', '_')
        self.assertLessEqual(len(label), 63)


class PackagedChartTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        supplied = os.environ.get('AI_SDLC_TEST_CHART_PACKAGE')
        if supplied:
            cls.directory = Path(supplied)
        else:
            cls.temporary = tempfile.TemporaryDirectory()
            cls.directory = Path(cls.temporary.name)
            chart_release.package(HELM, REVISION, cls.directory)
        cls.record = json.loads((cls.directory / 'chart-release.json').read_text())
        cls.archive = cls.directory / cls.record['archive']

    @classmethod
    def tearDownClass(cls):
        if hasattr(cls, 'temporary'):
            cls.temporary.cleanup()

    def helm(self, *args):
        result = subprocess.run([HELM, *args], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        return result.stdout

    def test_record_identifies_the_archive(self):
        record = self.record
        self.assertEqual(set(record), {'chart', 'version', 'tag', 'repository', 'revision', 'archive', 'sha256', 'size'})
        self.assertEqual(record['repository'], 'quay.io/os-santiago/ai-sdlc-k8s-runtime')
        self.assertEqual(record['version'], chart_release.published_version((CHART / 'Chart.yaml').read_text(), record['revision']))
        self.assertEqual(record['tag'], record['version'].replace('+', '_'))
        data = self.archive.read_bytes()
        self.assertEqual((len(data), hashlib.sha256(data).hexdigest()), (record['size'], record['sha256']))

    def test_packaged_chart_lints(self):
        self.helm('lint', '--strict', str(self.archive))
        self.helm('lint', '--strict', str(self.archive), '-f', str(LOCAL))

    def test_packaged_chart_renders_like_the_source_chart(self):
        """Only the helm.sh/chart label carries the published version, and the
        config checksum annotation changes with it because it hashes the
        labelled ConfigMaps."""
        label = re.compile(r'(?m)^(\s*(?:helm\.sh/chart|checksum/config): )\S+$')
        committed = chart_release.committed_version((CHART / 'Chart.yaml').read_text())
        for values in ([], ['-f', str(LOCAL)], ['-f', str(LOCAL), '--set', 'runtime.mode=shadow']):
            source = self.helm('template', 'ai-sdlc', str(CHART), '--namespace', 'ai-sdlc-runtime', *values)
            packaged = self.helm('template', 'ai-sdlc', str(self.archive), '--namespace', 'ai-sdlc-runtime', *values)
            self.assertIn('helm.sh/chart: ' + chart_release.CHART_NAME + '-' + self.record['tag'], packaged)
            self.assertIn('helm.sh/chart: ' + chart_release.CHART_NAME + '-' + committed, source)
            self.assertEqual(label.sub(r'\1-', packaged), label.sub(r'\1-', source))


class WorkflowTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.text = WORKFLOW.read_text()
        cls.workflow = yaml.safe_load(cls.text)
        cls.jobs = cls.workflow['jobs']

    def test_only_the_sign_job_can_request_an_oidc_token(self):
        self.assertEqual(self.workflow['permissions'], {'contents': 'read'})
        self.assertEqual(self.jobs['sign']['permissions'], {'contents': 'read', 'id-token': 'write'})
        for name, job in self.jobs.items():
            if name != 'sign':
                self.assertNotIn('id-token', job.get('permissions', {}), name)
                self.assertNotIn('write', set((job.get('permissions') or {}).values()), name)

    def test_publication_and_signing_run_only_for_main_pushes(self):
        main = "github.event_name == 'push' && github.ref == 'refs/heads/main'"
        self.assertEqual(self.jobs['publish']['if'], main)
        self.assertTrue(self.jobs['sign']['if'].startswith(main + ' && '))
        self.assertIn("needs.publish.outputs.published == 'true'", self.jobs['sign']['if'])
        self.assertEqual(self.workflow[True]['push']['branches'], ['main'])

    def test_sign_job_runs_no_third_party_or_repository_code(self):
        steps = self.jobs['sign']['steps']
        actions = {step['uses'].split('@')[0] for step in steps if 'uses' in step}
        self.assertEqual(actions, {'actions/download-artifact'})
        for step in steps:
            if 'uses' in step:
                self.assertRegex(step['uses'], r'@[0-9a-f]{40}$')
            self.assertNotIn('tools/', step.get('run', ''))
            self.assertNotIn('install.sh', step.get('run', ''))

    def test_cosign_pin_matches_the_tool_lock(self):
        lock = json.loads(LOCK.read_text())['cosign']
        step = next(s for s in self.jobs['sign']['steps'] if 'COSIGN_URL' in s.get('env', {}))
        self.assertEqual(step['env'], {'COSIGN_URL': lock['url'], 'COSIGN_SHA256': lock['sha256']})
        self.assertIn('sha256sum -c -', step['run'])

    def test_signing_targets_a_digest_and_verifies_the_workflow_identity(self):
        runs = [s.get('run', '') for s in self.jobs['sign']['steps']]
        self.assertIn('cosign sign --yes "$CHART"', runs)
        self.assertIn("output.write('chart=' + identity['repository'] + '@' + identity['digest'] + '\\n')", self.text)
        verify = next(r for r in runs if r.startswith('cosign verify'))
        self.assertIn('--certificate-identity "' + IDENTITY + '"', verify)
        self.assertIn('--certificate-github-workflow-sha "$GITHUB_SHA"', verify)

    def test_workflow_and_guide_publish_to_the_tool_repository(self):
        self.assertIn('oci://' + chart_release.REGISTRY_NAMESPACE, self.text)
        self.assertIn(chart_release.REPOSITORY.split('/', 1)[1], self.text)
        guide = GUIDE.read_text()
        self.assertIn(chart_release.REPOSITORY, guide)
        self.assertIn(IDENTITY, guide)
        self.assertIn('--certificate-oidc-issuer https://token.actions.githubusercontent.com', guide)

    def test_package_job_tests_the_packaged_archive(self):
        runs = '\n'.join(s.get('run', '') for s in self.jobs['package']['steps'])
        self.assertIn('tools/chart_release.py package', runs)
        self.assertIn('AI_SDLC_TEST_CHART_PACKAGE', runs)
        self.assertIn("-p 'test_helm_chart_*.py'", runs)


if __name__ == '__main__':
    unittest.main()
