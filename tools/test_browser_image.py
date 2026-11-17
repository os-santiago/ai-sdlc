import hashlib
import json
import unittest

import browser_image
from verify_browser_component import guarded_call


class BrowserImage(unittest.TestCase):
    def graph(self):
        blobs = {}
        def add(value):
            raw = json.dumps(value).encode()
            digest = 'sha256:'+hashlib.sha256(raw).hexdigest()
            blobs[digest] = raw
            return {'digest': digest, 'size': len(raw)}
        config = add({'architecture': 'amd64', 'os': 'linux'})
        manifest = add({'schemaVersion': 2, 'config': config, 'layers': []})
        index = add({'schemaVersion': 2, 'manifests': [dict(manifest, platform={'os': 'linux', 'architecture': 'amd64'})]})
        return blobs, add, index['digest'], manifest['digest']

    def test_hash_verified_index_and_manifest_are_same_image(self):
        blobs, _, index, manifest = self.graph()
        read = lambda digest, _: blobs[digest]
        for reference, runtime in ((index, manifest), (manifest, index), (manifest, manifest)):
            result = browser_image.verify(reference, runtime, read)
            self.assertEqual(result['manifestDigest'], manifest)
            self.assertTrue(result['metadataHashesVerified'])

    def test_foreign_manifest_tampering_platform_and_nested_index_rejected(self):
        blobs, add, index, manifest = self.graph()
        other = add({'schemaVersion': 2, 'config': add({'os': 'linux', 'architecture': 'amd64', 'other': True}), 'layers': []})['digest']
        nested = add({'schemaVersion': 2, 'manifests': [{'digest': index, 'size': len(blobs[index])}]})['digest']
        wrong = add({'schemaVersion': 2, 'config': add({'os': 'linux', 'architecture': 'arm64'}), 'layers': []})['digest']
        for reference, runtime in ((index, other), (nested, manifest), (wrong, wrong)):
            with self.assertRaises(ValueError): browser_image.verify(reference, runtime, lambda digest, _: blobs[digest])
        with self.assertRaises(ValueError): browser_image.verify(index, manifest, lambda digest, _: blobs[digest]+b' ')

    def test_published_index_allows_only_bound_nonrunnable_attestation(self):
        blobs, add, _, manifest = self.graph()
        image = {'digest': manifest, 'size': len(blobs[manifest]), 'platform': {'os': 'linux', 'architecture': 'amd64'}}
        attestation = dict(add({'schemaVersion': 2, 'layers': []}),
            mediaType='application/vnd.oci.image.manifest.v1+json',
            platform={'os': 'unknown', 'architecture': 'unknown'},
            annotations={'vnd.docker.reference.type': 'attestation-manifest', 'vnd.docker.reference.digest': manifest})
        for entries in ([image, attestation], [attestation, image]):
            index = add({'schemaVersion': 2, 'manifests': entries})['digest']
            self.assertEqual(browser_image.verify(index, manifest, lambda d, _: blobs[d])['manifestDigest'], manifest)
        for changed in (dict(attestation, platform={'os': 'linux', 'architecture': 'arm64'}),
                        dict(attestation, annotations={'vnd.docker.reference.type': 'attestation-manifest', 'vnd.docker.reference.digest': 'sha256:'+'a'*64}),
                        dict(attestation, size=131073), dict(attestation, annotations={}),
                        dict(attestation, platform={'os': 'unknown', 'architecture': 'amd64'})):
            index = add({'schemaVersion': 2, 'manifests': [image, changed]})['digest']
            with self.subTest(changed=changed), self.assertRaises(ValueError):
                browser_image.verify(index, manifest, lambda d, _: blobs[d])
        index = add({'schemaVersion': 2, 'manifests': [image, image]})['digest']
        with self.assertRaises(ValueError): browser_image.verify(index, manifest, lambda d, _: blobs[d])

    def test_scope_replacement_prevents_effect_and_invalidates_observation(self):
        namespace = 'fixture'
        for changed in ('namespace', 'cluster', 'deletion', 'label'):
            writes = []
            def call(args, value=None):
                if args[0] == 'create':
                    writes.append(value)
                    return '{}'
                cluster = args[2] == 'kube-system'
                metadata = {'uid': 'cluster' if cluster else 'original', 'labels': {'ai-sdlc.browser-fixture': namespace}}
                if changed == 'cluster' and cluster or changed == 'namespace' and not cluster:
                    metadata['uid'] = 'replacement'
                if changed == 'deletion' and not cluster: metadata['deletionTimestamp'] = 'now'
                if changed == 'label' and not cluster: metadata['labels'] = {}
                return json.dumps({'metadata': metadata})
            with self.subTest(changed=changed), self.assertRaises(ValueError):
                guarded_call(call, namespace, 'original', 'cluster', ['create', '-f', '-'], {'kind': 'Pod'})
            self.assertEqual(writes, [])
        observed = []
        def replaced_after_get(args, value=None):
            if args[1] == 'pods':
                observed.append(True)
                return '{}'
            return json.dumps({'metadata': {'uid': 'cluster' if args[2] == 'kube-system' else 'replacement' if observed else 'original',
                                           'labels': {'ai-sdlc.browser-fixture': namespace}}})
        with self.assertRaises(ValueError):
            guarded_call(replaced_after_get, namespace, 'original', 'cluster', ['get', 'pods', '-o', 'json'])


if __name__ == '__main__':
    unittest.main()
