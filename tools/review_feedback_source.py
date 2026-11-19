"""Read-only custody of a completed schema36 installation from later source."""
import copy

import bootstrap_prepare as bootstrap
import browser_upgrade_chart
from browser_approval_state import CurrentSource
import build_foundation_install as files
import install
import install_images
import install_upgrade
import upgrade_backup_data as data

VERSION = 'ai-sdlc.review-feedback-runtime-upgrade/v1'


class CompletedFeedbackSource:
    def __init__(self, config):
        self.config = config
        self.path = config.state_dir / 'review-feedback-runtime-upgrade.json'
        self.record = files.bounded_json(self.path, 32*1024*1024)
        record = self.record
        reports = record.get('installationReports', [])
        if (record.get('version') != VERSION or record.get('mode') != 'review-feedback'
                or record.get('completed') != 'active' or record.get('transition')
                or record.get('recoveryPause') or record.get('migrationVerified') is not True
                or (record.get('backup') or {}).get('phase') != 'verified'
                or record['backup'].get('restore', {}).get('state') != 'removed'
                or not reports or reports[-1].get('configuration') != 'active'
                or reports[-1].get('installationReady') is not True
                or reports[-1].get('reviewFeedbackUpgradeOperation') != record.get('nonce')):
            raise ValueError('Completed original feedback installation required')
        selection = record['selection']
        source = install_upgrade.Source(install.ROOT)
        catalog = source.catalog(selection['revision'])
        if (len(catalog) != 36 or catalog[-1]['filename'] != '0036_review_feedback.sql'
                or catalog != selection['catalog'] or record['compatibility'].get('catalogSize') != 36):
            raise ValueError('Exact original feedback catalog required')
        self.original_digest = bootstrap.digest(record)
        self.chart = browser_upgrade_chart.OriginalChart(install.ROOT, selection['revision'], record['chartSHA256'])
        self.browser = None
        try:
            with self.chart.selected():
                self.browser = CurrentSource(config)
            if self.browser.digest() != record['parentSHA256']:
                raise ValueError('Original feedback parent custody changed')
            self.parent, self.pause = self.browser.parent, self.browser.pause
            self.helper = install_images.REGISTRY + '@' + selection['images']['migrator']['digest']
            self.custody()
        except BaseException:
            self.close()
            raise

    def digest(self):
        return self.original_digest

    def custody(self):
        if bootstrap.digest(files.bounded_json(self.path, 32*1024*1024)) != self.original_digest:
            raise ValueError('Original completed feedback journal changed')
        with self.chart.selected():
            self.browser.custody()
        if self.browser.digest() != self.record['parentSHA256']:
            raise ValueError('Original feedback ancestry changed')
        data.verify_archives(self.config.state_dir / 'review-feedback-runtime-backup', self.record['backup']['archives'])

    @property
    def current_report(self):
        return copy.deepcopy(self.record['installationReports'][-1])

    @property
    def browser_compiled(self):
        return copy.deepcopy(self.record['browserCompiled'])

    def describe(self, phase):
        if phase != 'active':
            raise ValueError('Only a completed feedback source is selectable')
        self.custody()
        return copy.deepcopy(self.record['targets']['active'])

    def close(self):
        if self.browser is not None:
            self.browser.close()
        self.chart.close()
