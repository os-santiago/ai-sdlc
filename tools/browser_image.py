"""Bind a fixture reference and CRI runtime digest to one hash-verified image."""
import hashlib
import json
import re


def verify(reference, runtime, read):
    cache = {}
    def blob(digest, size=None):
        if not isinstance(digest, str) or not re.fullmatch('sha256:[a-f0-9]{64}', digest):
            raise ValueError('Invalid browser descriptor')
        if size is not None and (type(size) is not int or not 0 < size <= 131072):
            raise ValueError('Browser metadata exceeds bound')
        if digest not in cache:
            raw = read(digest, size or 131072)
            if not isinstance(raw, bytes) or not 0 < len(raw) <= 131072 or 'sha256:'+hashlib.sha256(raw).hexdigest() != digest:
                raise ValueError('Browser metadata digest differs')
            cache[digest] = raw
        if size is not None and len(cache[digest]) != size:
            raise ValueError('Browser descriptor size differs')
        return json.loads(cache[digest])
    def manifest(digest):
        document = blob(digest)
        if document.get('schemaVersion') != 2:
            raise ValueError('Browser schema differs')
        if 'manifests' in document:
            entries = document['manifests']
            if not isinstance(entries, list) or not 1 <= len(entries) <= 2 or any(not isinstance(item, dict) for item in entries):
                raise ValueError('Single browser platform required')
            images = [item for item in entries if item.get('platform', {}).get('os') != 'unknown']
            if len(images) != 1:
                raise ValueError('Single browser platform required')
            item = images[0]
            if 'platform' in item and (item['platform'].get('os') != 'linux' or item['platform'].get('architecture') != 'amd64'):
                raise ValueError('Browser descriptor platform differs')
            # BuildKit's publication index also binds a non-runnable
            # attestation descriptor. It is not an alternative runtime image
            # or verification of the attestation's claims.
            for other in entries:
                if other is item:
                    continue
                annotations = other.get('annotations', {})
                if (other.get('platform') != {'os': 'unknown', 'architecture': 'unknown'}
                        or other.get('mediaType') != 'application/vnd.oci.image.manifest.v1+json'
                        or annotations.get('vnd.docker.reference.type') != 'attestation-manifest'
                        or annotations.get('vnd.docker.reference.digest') != item['digest']
                        or not re.fullmatch('sha256:[a-f0-9]{64}', other.get('digest', ''))
                        or type(other.get('size')) is not int or not 0 < other['size'] <= 131072):
                    raise ValueError('Browser attestation descriptor differs')
            digest = item['digest']
            document = blob(digest, item['size'])
            if document.get('schemaVersion') != 2 or 'manifests' in document:
                raise ValueError('Nested browser index rejected')
        return digest, document
    selected, document = manifest(reference)
    actual, actual_document = manifest(runtime)
    if actual != selected or actual_document != document:
        raise ValueError('Browser runtime is not the selected manifest')
    config = document['config']
    platform = blob(config['digest'], config['size'])
    if platform.get('os') != 'linux' or platform.get('architecture') != 'amd64':
        raise ValueError('Browser image platform differs')
    return {'referenceDigest': reference, 'runtimeDigest': runtime, 'manifestDigest': selected,
            'configurationDigest': config['digest'], 'metadataHashesVerified': True}
