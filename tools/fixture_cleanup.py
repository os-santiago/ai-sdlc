"""Failure-preserving cleanup for explicitly isolated administrative fixtures."""
import json
import re


def recover_namespace_uid(call, namespace, label, nonce):
    """Recover only a cleanup identity after an attempted create; never resume work."""
    raw = call(['get', 'namespace', namespace, '--ignore-not-found', '-o', 'json'])
    if not raw.strip():
        return None
    metadata = json.loads(raw)['metadata']
    uid = metadata.get('uid', '')
    if (metadata.get('name') != namespace or metadata.get('labels', {}).get(label) != nonce
            or not re.fullmatch(r'[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}', uid)):
        raise ValueError('Refusing cleanup identity from an unrelated namespace')
    return uid


def finish_fixture(report, persist, cleanup):
    """Report write errors cannot suppress cleanup; cleanup errors remain visible."""
    write_errors = []

    def checkpoint():
        try:
            persist()
        except OSError as exc:
            write_errors.append(exc)
            report['status'] = 'failed'
            report['reportWriteFailure'] = type(exc).__name__

    checkpoint()
    try:
        cleanup(checkpoint)
    except BaseException as exc:
        report['status'] = 'failed'
        report['cleanupComplete'] = False
        report['cleanupFailure'] = type(exc).__name__
        raise
    finally:
        checkpoint()
    if write_errors:
        raise write_errors[0]
