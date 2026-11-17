"""Bounded approval history above an immutable completed browser installation.

Completed administrative records are never rewritten. This is local installer
custody, not a new source of authority for historical workflow requests.
"""
import copy
import json
import re

import bootstrap_prepare as bootstrap
import build_foundation_install as files
import konflux_runtime_install as platform

VERSION = 'ai-sdlc.browser-approval-history/v1'
OPERATION = 'ai-sdlc.browser-approval-window/v1'
MAX_OPERATIONS = 8
PHASES = ('source', 'paused', 'ready', 'staged', 'warming', 'armed', 'active')


def source_digest(browser_digest, operations):
    if not operations:
        # Preserve the existing feedback protocol when no renewal has occurred.
        return browser_digest
    return bootstrap.digest({'browserSHA256': browser_digest, 'approvals': operations})


def validate(value, browser_digest):
    if (not isinstance(value, dict)
            or set(value) != {'version', 'browserSHA256', 'operations'}
            or value['version'] != VERSION or value['browserSHA256'] != browser_digest
            or not isinstance(value['operations'], list)
            or len(value['operations']) > MAX_OPERATIONS):
        raise ValueError('Original bounded browser approval history required')
    seen, documents, prefix = set(), set(), []
    for index, operation in enumerate(value['operations']):
        if not isinstance(operation, dict):
            raise ValueError('Invalid browser approval operation')
        nonce = operation.get('nonce', '')
        approvals = operation.get('approvals')
        if (operation.get('version') != OPERATION
                or not isinstance(nonce, str) or not re.fullmatch('[a-f0-9]{32}', nonce)
                or nonce in seen or operation.get('mode') != 'browser-approval'
                or operation.get('parentSHA256') != source_digest(browser_digest, prefix)
                or operation.get('sourceApprovalCount') != index
                or operation.get('completed') not in PHASES
                or not isinstance(approvals, dict) or set(approvals) != {'provenance', 'release'}
                or operation.get('approvalsSHA256') != bootstrap.digest(approvals)):
            raise ValueError('Browser approval history identity changed')
        digest = bootstrap.digest(approvals)
        if digest in documents:
            raise ValueError('Historical approval documents cannot create another operation')
        seen.add(nonce)
        documents.add(digest)
        if type(operation.get('accepted')) is not bool:
            raise ValueError('Explicit browser approval acceptance state required')
        complete = operation['accepted']
        if complete and operation['completed'] != 'active':
            raise ValueError('Only the active approval phase can be accepted')
        if complete and (operation.get('transition') or operation.get('recoveryPause')
                or (operation.get('backup') or {}).get('phase') != 'verified'
                or operation['backup'].get('restore', {}).get('state') != 'removed'
                or not operation.get('installationReports')
                or operation['installationReports'][-1].get('installationReady') is not True
                or operation['installationReports'][-1].get('configuration') != 'active'):
            raise ValueError('Completed browser approval lacks original acceptance')
        if index < len(value['operations']) - 1 and not complete:
            raise ValueError('Incomplete browser approval cannot acquire a descendant')
        prefix.append(operation)
    return value


class Journal:
    def __init__(self, state_dir, browser_digest):
        self.path = state_dir / 'browser-approval-history.json'
        self.browser_digest = browser_digest
        self.value = self.read()
        self.original_digest = bootstrap.digest(self.value)

    def read(self):
        if self.path.exists() or self.path.is_symlink():
            value = files.bounded_json(self.path, 32 * 1024 * 1024)
        else:
            value = {'version': VERSION, 'browserSHA256': self.browser_digest, 'operations': []}
        return validate(value, self.browser_digest)

    def custody(self):
        if bootstrap.digest(self.read()) != self.original_digest:
            raise ValueError('Original browser approval journal changed')

    def select(self, approvals):
        """Return the last matching operation, or a new-operation position."""
        self.custody()
        operations = self.value['operations']
        if approvals is None:
            if not operations:
                raise ValueError('No recorded browser approval operation')
            return len(operations) - 1
        for index, operation in enumerate(operations):
            if operation['approvals'] == approvals:
                if index != len(operations) - 1:
                    raise ValueError('Historical approval operation cannot be resumed')
                return index
        if operations and not operations[-1]['accepted']:
            raise ValueError('Incomplete operation cannot adopt different approval documents')
        if len(operations) >= MAX_OPERATIONS:
            raise ValueError('Browser approval history reached its reviewed bound')
        return len(operations)

    def save(self, position, record):
        self.custody()
        candidate = copy.deepcopy(self.value)
        operations = candidate['operations']
        if position == len(operations):
            operations.append(copy.deepcopy(record))
        elif position == len(operations) - 1:
            original = operations[-1]
            if original['accepted'] and original != record:
                raise ValueError('Completed browser approval record is immutable')
            for key in ('nonce', 'parentSHA256', 'sourceApprovalCount', 'createdAt', 'expiresAt',
                        'approvals', 'approvalsSHA256', 'helperSelection', 'targets',
                        'sourceProof', 'sourceCompatibility', 'sourceRoleIdentities',
                        'installationSource', 'browserCompiled', 'resourceUIDs'):
                if original.get(key) != record.get(key):
                    raise ValueError('Original browser approval intent changed')
            if ('fencedSourceProof' in original
                    and original['fencedSourceProof'] != record.get('fencedSourceProof')):
                raise ValueError('Original fenced browser approval history changed')
            operations[-1] = copy.deepcopy(record)
        else:
            raise ValueError('Only the latest approval operation may progress')
        validate(candidate, self.browser_digest)
        if len(json.dumps(candidate, sort_keys=True).encode()) + 1 > 32 * 1024 * 1024:
            raise ValueError('Browser approval history exceeds its reviewed byte bound')
        platform.write_record(self.path, candidate)
        self.value = candidate
        self.original_digest = bootstrap.digest(candidate)


class CurrentSource:
    """Separate historical custody from the current approved schema35 render."""
    def __init__(self, config, count=None, allowed_nonce=None):
        from browser_runtime_upgrade import BrowserUpgrade
        self.browser = BrowserUpgrade(config)
        try:
            self.record = self.browser.record
            if (not self.record or self.record.get('completed') != 'active'
                    or self.record.get('mode') != 'browser-evidence'
                    or not self.record.get('migrationVerified') or self.record.get('transition')
                    or self.record.get('recoveryPause')
                    or (self.record.get('backup') or {}).get('phase') != 'verified'
                    or self.record['backup'].get('restore', {}).get('state') != 'removed'):
                raise ValueError('Original completed schema35 browser installation required')
            self.original_digest = bootstrap.digest(self.record)
            self.journal = Journal(config.state_dir, self.original_digest)
            operations = self.journal.value['operations']
            self.count = len(operations) if count is None else count
            if type(self.count) is not int or not 0 <= self.count <= len(operations):
                raise ValueError('Invalid original browser approval prefix')
            self.prefix = copy.deepcopy(operations[:self.count])
            self.allowed_nonce = allowed_nonce
            if any(not op['accepted'] for op in self.prefix):
                raise ValueError('Incomplete approval is not an installed source')
            self.parent, self.pause = self.browser.parent, self.browser.pause
            self.helper, self.path = self.browser.helper, self.browser.path
            self.custody()
        except BaseException:
            self.browser.close()
            raise

    def digest(self):
        return source_digest(self.original_digest, self.prefix)

    def custody(self):
        self.browser.custody()
        if bootstrap.digest(files.bounded_json(self.path, 32 * 1024 * 1024)) != self.original_digest:
            raise ValueError('Original browser record changed')
        operations = self.journal.read()['operations']
        if operations[:self.count] != self.prefix:
            raise ValueError('Original browser approval prefix changed')
        tail = operations[self.count:]
        if tail and (len(tail) != 1 or self.allowed_nonce is None
                     or tail[0]['nonce'] != self.allowed_nonce):
            raise ValueError('Another approval operation changed the installed source')

    @property
    def current_report(self):
        reports = (self.prefix[-1] if self.prefix else self.record).get('installationReports', [])
        if not reports:
            raise ValueError('Original browser source report unavailable')
        return copy.deepcopy(reports[-1])

    def describe(self, phase):
        if phase != 'active':
            raise ValueError('Only the completed browser source is selectable')
        return copy.deepcopy(self.prefix[-1]['targets']['active']) if self.prefix else self.browser.describe('active')

    def close(self):
        self.browser.close()
