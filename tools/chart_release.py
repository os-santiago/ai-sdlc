#!/usr/bin/env python3
"""Package the runtime Helm chart at its deterministic published version.

The published version is the committed Chart.yaml version plus the first 12
hex digits of the source revision as SemVer build metadata, for example
``0.4.0+0e3b526f1a2b``. OCI tags cannot contain ``+``, so Helm stores that
version under the tag ``0.4.0_0e3b526f1a2b``. The same revision always yields
the same version; the signature certificate carries the full revision.

The archive bytes are reproducible: after ``helm package`` the archive is
rewritten with sorted members, a fixed modification time (the commit time,
passed as the source date epoch), root ownership, normalized modes and a
gzip header without a timestamp. Packaging the same commit twice, whatever
the files' mtimes, yields the same SHA-256.

Only the standard library is used. This tool never pushes or signs: the chart
workflow pushes in a job without an OIDC token, and signs in a separate job
that runs no repository code.
"""
import argparse
import gzip
import hashlib
import io
import os
import json
from pathlib import Path
import re
import subprocess
import sys
import tarfile

ROOT = Path(__file__).resolve().parents[1]
CHART = ROOT / 'runtime/k8s/deploy/helm/ai-sdlc-k8s-runtime'
CHART_NAME = 'ai-sdlc-k8s-runtime'
REGISTRY_NAMESPACE = 'quay.io/os-santiago'
REPOSITORY = REGISTRY_NAMESPACE + '/' + CHART_NAME
REVISION_DIGITS = 12
MAX_PACKAGE_BYTES = 4 * 1024 * 1024


def committed_version(chart_yaml):
    """The top-level ``version:`` of Chart.yaml; it must be plain MAJOR.MINOR.PATCH."""
    found = re.findall(r'(?m)^version:[ \t]*["\']?([^"\'\s#]+)["\']?[ \t]*(?:#.*)?$', chart_yaml)
    if len(found) != 1:
        raise ValueError('Chart.yaml must declare exactly one top-level version')
    if not re.fullmatch(r'(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)', found[0]):
        raise ValueError('Chart.yaml version must be MAJOR.MINOR.PATCH without pre-release or build metadata')
    return found[0]


def published_version(chart_yaml, revision):
    if not isinstance(revision, str) or not re.fullmatch(r'[0-9a-f]{40}', revision):
        raise ValueError('The revision must be a full 40-digit lowercase Git commit SHA')
    return committed_version(chart_yaml) + '+' + revision[:REVISION_DIGITS]


def oci_tag(version):
    """Helm's OCI tag for a chart version: ``+`` is not a valid tag character."""
    return version.replace('+', '_')


def normalize(archive, epoch):
    """Rewrite a packaged chart so that its bytes depend only on file contents."""
    if type(epoch) is not int or not 0 <= epoch < 2 ** 33:
        raise ValueError('The source date epoch must be a non-negative integer')
    files = {}
    with tarfile.open(archive, 'r:gz') as bundle:
        for member in bundle.getmembers():
            if member.isdir():
                continue
            if not member.isfile() or member.name in files:
                raise ValueError('Packaged chart contains a non-regular or duplicate entry: ' + member.name)
            files[member.name] = bundle.extractfile(member).read()
    raw = io.BytesIO()
    with tarfile.open(fileobj=raw, mode='w', format=tarfile.GNU_FORMAT) as bundle:
        for name in sorted(files):
            info = tarfile.TarInfo(name)
            info.size, info.mtime, info.mode = len(files[name]), epoch, 0o644
            info.uid = info.gid = 0
            info.uname = info.gname = ''
            bundle.addfile(info, io.BytesIO(files[name]))
    compressed = io.BytesIO()
    with gzip.GzipFile(filename='', mode='wb', fileobj=compressed, mtime=0, compresslevel=9) as stream:
        stream.write(raw.getvalue())
    Path(archive).write_bytes(compressed.getvalue())


def package(helm, revision, output, epoch, chart=CHART):
    """Package the chart with ``helm package --version`` and return its release record."""
    version = published_version((Path(chart) / 'Chart.yaml').read_text(), revision)
    output = Path(output)
    output.mkdir(parents=True, exist_ok=True)
    subprocess.run([str(helm), 'package', str(chart), '--version', version, '--destination', str(output)],
                   check=True, stdout=subprocess.DEVNULL)
    archive = output / (CHART_NAME + '-' + version + '.tgz')
    normalize(archive, epoch)
    data = archive.read_bytes()
    if not 0 < len(data) <= MAX_PACKAGE_BYTES:
        raise ValueError('Packaged chart size is outside its bound')
    with tarfile.open(archive, 'r:gz') as bundle:
        names = bundle.getnames()
        if any(not name.startswith(CHART_NAME + '/') or '..' in Path(name).parts for name in names):
            raise ValueError('Packaged chart contains a path outside its chart directory')
        packaged = bundle.extractfile(CHART_NAME + '/Chart.yaml').read().decode()
    if re.findall(r'(?m)^version:[ \t]*(\S+)[ \t]*$', packaged) != [version]:
        raise ValueError('Packaged Chart.yaml does not carry the published version')
    record = {'chart': CHART_NAME, 'version': version, 'tag': oci_tag(version),
              'repository': REPOSITORY, 'revision': revision, 'archive': archive.name,
              'sha256': hashlib.sha256(data).hexdigest(), 'size': len(data)}
    (output / 'chart-release.json').write_text(json.dumps(record, sort_keys=True) + '\n')
    return record


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    commands = parser.add_subparsers(dest='command', required=True)
    version = commands.add_parser('version', help='print the published version for a revision')
    version.add_argument('--revision', required=True)
    build = commands.add_parser('package', help='package the chart and write chart-release.json')
    build.add_argument('--helm', required=True, type=Path)
    build.add_argument('--revision', required=True)
    build.add_argument('--output', required=True, type=Path)
    build.add_argument('--source-date-epoch', type=int, default=None,
                       help='archive modification time; default: the SOURCE_DATE_EPOCH environment variable')
    args = parser.parse_args(argv)
    if args.command == 'version':
        value = published_version((CHART / 'Chart.yaml').read_text(), args.revision)
        print(json.dumps({'chart': CHART_NAME, 'version': value, 'tag': oci_tag(value), 'repository': REPOSITORY}))
    else:
        epoch = args.source_date_epoch
        if epoch is None:
            if not re.fullmatch(r'[0-9]{1,11}', os.environ.get('SOURCE_DATE_EPOCH', '')):
                raise ValueError('Pass --source-date-epoch or set SOURCE_DATE_EPOCH (for example the commit time)')
            epoch = int(os.environ['SOURCE_DATE_EPOCH'])
        print(json.dumps(package(args.helm, args.revision, args.output, epoch), sort_keys=True))
    return 0


if __name__ == '__main__':
    try:
        sys.exit(main())
    except (ValueError, OSError, subprocess.SubprocessError) as error:
        print('Chart packaging failed: ' + str(error), file=sys.stderr)
        sys.exit(1)
