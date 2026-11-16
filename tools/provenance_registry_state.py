"""Administrative registry change journal; never workflow-stage authority.

The caller owns cluster/freeze/containment fences and protected credential
custody. This journal records only identities and content digests. It never
rotates credentials, restores permissive ACLs or replaces Pods.
"""
import copy
import hashlib
import json
import re
import time

from konflux_runtime_install import write_record

UID = re.compile(r'[a-f0-9]{8}(?:-[a-f0-9]{4}){3}-[a-f0-9]{12}')
CID = re.compile(r'containerd://[a-f0-9]{64}')


def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(',', ':')).encode()).hexdigest()


def content(resource):
    value = copy.deepcopy(resource)
    meta = value.get('metadata', {})
    for field in ('uid', 'resourceVersion', 'creationTimestamp', 'managedFields', 'generation'):
        meta.pop(field, None)
    return value


def resource_identity(resource):
    meta = resource.get('metadata', {})
    if (resource.get('apiVersion') != 'v1' or resource.get('kind') not in ('ConfigMap', 'Secret')
            or not UID.fullmatch(meta.get('uid', '')) or meta.get('deletionTimestamp')
            or not re.fullmatch('[0-9]+', str(meta.get('resourceVersion', '')))
            or not isinstance(resource.get('data'), dict)):
        raise ValueError('Original registry configuration identity required')
    return '/'.join((resource['kind'], meta.get('namespace', ''), meta.get('name', '')))


class RegistryJournal:
    def __init__(self, path, state, fence):
        self.path, self.state, self.fence = path, state, fence
        if (state.get('version') != 'ai-sdlc.registry-upgrade-journal/v1'
                or not isinstance(state.get('changes'), dict) or not isinstance(state.get('restarts'), dict)):
            raise ValueError('Invalid registry upgrade journal')

    def save(self, candidate):
        # Publish only after fsync+rename+directory-fsync succeed. Other journal
        # instances may share this dictionary, so an instance-only error latch
        # would not protect them from an unpersisted in-memory intent.
        write_record(self.path, candidate)
        self.state.update(candidate)

    def save_entry(self, section, key, value):
        candidate = copy.deepcopy(self.state)
        candidate[section][key] = copy.deepcopy(value)
        self.save(candidate)

    def change_data(self, original, desired_data, read, replace):
        """One UID/resourceVersion CAS; resume an exact result after lost ACK.

        Original and desired data must come from protected, previously recorded
        installation inputs. read/replace return complete Kubernetes objects.
        """
        key = resource_identity(original)
        desired = copy.deepcopy(original)
        desired['data'] = copy.deepcopy(desired_data)
        if not isinstance(desired_data, dict) or original['data'] == desired_data:
            raise ValueError('Explicit registry data transition required')
        expected = {'uid': original['metadata']['uid'], 'beforeVersion': original['metadata']['resourceVersion'],
                    'beforeSHA256': digest(content(original)), 'afterSHA256': digest(content(desired))}
        intent = copy.deepcopy(self.state['changes'].get(key))
        self.fence()
        observed = read()
        if resource_identity(observed) != key or observed['metadata']['uid'] != expected['uid']:
            raise ValueError('Registry resource was replaced')
        if intent is None:
            if digest(content(observed)) != expected['beforeSHA256'] or observed['metadata']['resourceVersion'] != expected['beforeVersion']:
                raise ValueError('Original registry configuration drifted before intent')
            intent = dict(expected, phase='prepared', afterVersion=None)
            self.save_entry('changes', key, intent)
        elif any(intent.get(k) != v for k, v in expected.items()) or intent.get('phase') not in ('prepared', 'applied'):
            raise ValueError('Recorded registry transition changed')
        current_hash = digest(content(observed))
        current_version = observed['metadata']['resourceVersion']
        if intent['phase'] == 'applied':
            if current_hash != expected['afterSHA256'] or current_version != intent['afterVersion']:
                raise ValueError('Applied registry configuration changed; no rollback allowed')
            self.fence()
            return observed
        if current_hash == expected['beforeSHA256'] and current_version == expected['beforeVersion']:
            replacement = copy.deepcopy(observed)
            replacement['data'] = copy.deepcopy(desired_data)
            # Recheck after the durable write, immediately before the effect.
            self.fence()
            replace(replacement)  # Original UID and resourceVersion are retained.
            observed = read()
            if resource_identity(observed) != key or observed['metadata']['uid'] != expected['uid']:
                raise ValueError('Registry resource was replaced during update')
            current_hash, current_version = digest(content(observed)), observed['metadata']['resourceVersion']
        if current_hash != expected['afterSHA256'] or current_version == expected['beforeVersion']:
            raise ValueError('Registry update outcome differs from recorded intent')
        self.fence()
        intent.update(phase='applied', afterVersion=current_version)
        self.save_entry('changes', key, intent)
        return observed

    def restart_original(self, key, configuration_sha256, observe, signal):
        """Resume a bounded original-task restart without signaling a successor.

        Returns False while an observed task is stopping. At most two signal
        attempts may target the recorded original task, each requiring a fresh
        positive observation and fence. Exhausted ambiguity is quarantined.
        """
        if not re.fullmatch('[a-z][a-z0-9-]{0,63}', key) or not re.fullmatch('[a-f0-9]{64}', configuration_sha256):
            raise ValueError('Invalid registry restart identity')
        self.fence()
        current = observe()
        required = {'podUID', 'containerID', 'restartCount', 'volumeIdentity', 'configurationSHA256', 'ready', 'running'}
        if (not isinstance(current, dict) or set(current) != required or not UID.fullmatch(current.get('podUID', ''))
                or not re.fullmatch('[0-9]+:[0-9]+', current.get('volumeIdentity', ''))
                or current.get('configurationSHA256') != configuration_sha256
                or type(current.get('restartCount')) is not int or current['restartCount'] < 0
                or type(current.get('ready')) is not bool or type(current.get('running')) is not bool):
            raise ValueError('Registry restart observation denied')
        intent = copy.deepcopy(self.state['restarts'].get(key))
        if intent is None:
            if not current['ready'] or not current['running'] or not CID.fullmatch(current.get('containerID', '')):
                raise ValueError('Original ready registry task required')
            intent = {'original': copy.deepcopy(current), 'attempts': 0, 'lastSignalAt': None, 'successor': None}
            self.save_entry('restarts', key, intent)
        original = intent['original']
        if any(current[k] != original[k] for k in ('podUID', 'volumeIdentity', 'configurationSHA256')):
            raise ValueError('Original registry Pod, volume or configuration changed')
        if intent['successor'] is not None:
            if current != intent['successor']:
                raise ValueError('Confirmed registry successor changed')
            self.fence()
            return True
        if current['containerID'] != original['containerID']:
            if not current['ready'] or not current['running']:
                return False
            if not CID.fullmatch(current.get('containerID', '')) or current['restartCount'] != original['restartCount'] + 1 or intent['attempts'] < 1:
                raise ValueError('Unexplained registry successor')
            self.fence()
            intent['successor'] = copy.deepcopy(current)
            self.save_entry('restarts', key, intent)
            return True
        if current['restartCount'] != original['restartCount']:
            raise ValueError('Original task restart counter changed')
        if not current['ready'] or not current['running']:
            return False
        if intent['lastSignalAt'] is not None and time.time() - intent['lastSignalAt'] < 5:
            return False
        if intent['attempts'] >= 2:
            raise ValueError('Original registry signal outcome remains uncertain')
        intent['attempts'] += 1
        intent['lastSignalAt'] = time.time()
        self.save_entry('restarts', key, intent)
        self.fence()
        # This argument is the persisted task ID, never the name or a successor.
        signal(original['containerID'])
        return False
