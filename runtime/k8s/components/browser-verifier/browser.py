"""Closed Saferus UI recipe; invoked only by a bounded deployer-owned workload."""
import hashlib
import json
import os
from pathlib import Path
import re
import resource
import subprocess
import sys
import tempfile
import threading
import time

ORIGIN = 'https://saferus-console:8443'
GET_PATHS = {'/', '/static/app.js', '/static/ttl.js', '/static/style.css', '/favicon.ico'}
FIELDS = {'version', 'requestID', 'targetSHA256', 'snapshotSHA256', 'imageDigest',
          'issuedAt', 'expiresAt', 'expectation'}
BOUNDARY = 'request'


def checkpoint(name):
    global BOUNDARY
    BOUNDARY = name  # Fixed call-site strings only; never error or response text.


def unique(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError('duplicate field')
        result[key] = value
    return result


def parse_request(raw, now):
    if not isinstance(raw, bytes) or not 0 < len(raw) <= 4096:
        raise ValueError('request size')
    value = json.loads(raw, object_pairs_hook=unique)
    if (not isinstance(value, dict) or set(value) != FIELDS
            or value['version'] != 'ai-sdlc.saferus-browser-request/v1'
            or not isinstance(value['requestID'], str)
            or not re.fullmatch('browser-[a-f0-9]{64}', value['requestID'])
            or value['expectation'] not in ('baseline-seconds', 'readable-duration')
            or any(not isinstance(value[key], str) or not re.fullmatch('[a-f0-9]{64}', value[key])
                   for key in ('targetSHA256', 'snapshotSHA256'))
            or not isinstance(value['imageDigest'], str)
            or not re.fullmatch('sha256:[a-f0-9]{64}', value['imageDigest'])
            or any(type(value[key]) is not int for key in ('issuedAt', 'expiresAt'))
            or not 0 < value['issuedAt'] <= now + 5000
            or not now + 1000 < value['expiresAt'] <= value['issuedAt'] + 30000):
        raise ValueError('request authority')
    return value


class NetworkGuard:
    """The browser can fetch only fixed assets and two exact demo requests."""
    def __init__(self):
        self.requests = 0
        self.posts = 0
        self.denied = False

    def allow(self, method, url, body):
        self.requests += 1
        accepted = False
        if self.requests <= 16:
            if method == 'GET' and url in {ORIGIN + path for path in GET_PATHS} and body is None:
                accepted = True
            elif method == 'POST' and url == ORIGIN + '/api/demo' and body == '{}' and self.posts < 2:
                self.posts += 1
                accepted = True
        if not accepted:
            self.denied = True
        return accepted


def api_result(raw):
    if not isinstance(raw, bytes) or not 0 < len(raw) <= 4096:
        raise ValueError('demo response size')
    result = json.loads(raw, object_pairs_hook=unique)
    if (not isinstance(result, dict)
            or set(result) != {'verified', 'algorithm', 'tokenTTLSeconds', 'tokenRevoked', 'agentless'}
            or result['verified'] is not True or result['tokenRevoked'] is not True
            or result['agentless'] is not True or result['algorithm'] != 'AES-256-GCM'
            or type(result['tokenTTLSeconds']) is not int or result['tokenTTLSeconds'] != 60):
        raise ValueError('demo response')
    return hashlib.sha256(raw).hexdigest()


def run(request, request_raw, ca_file):
    from playwright.sync_api import sync_playwright, expect
    started = int(time.time() * 1000)
    guard = NetworkGuard()
    observations = []
    expected = '60 seconds' if request['expectation'] == 'baseline-seconds' else '1 minute'
    checkpoint('trust-store')
    for directory in ('/work/home', '/work/tmp'):
        Path(directory).mkdir(mode=0o700, parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix='browser-', dir='/work/tmp') as profile:
        for command in (
            ['certutil', '-N', '-d', 'sql:' + profile, '--empty-password'],
            ['certutil', '-A', '-d', 'sql:' + profile, '-n', 'approved-console-ca', '-t', 'C,,', '-i', str(ca_file)],
        ):
            subprocess.run(command, check=True, capture_output=True, timeout=3)
        with sync_playwright() as playwright:
            checkpoint('browser-start')
            context = playwright.firefox.launch_persistent_context(profile,
                ignore_https_errors=False, accept_downloads=False, service_workers='block',
                timeout=8000, firefox_user_prefs={
                    'security.enterprise_roots.enabled': False,
                    'security.certerrors.mitm.auto_enable_enterprise_roots': False,
                    'network.trr.mode': 5, 'network.dns.disablePrefetch': True,
                    'network.prefetch-next': False, 'media.peerconnection.enabled': False,
                    'datareporting.healthreport.uploadEnabled': False,
                    'toolkit.telemetry.enabled': False,
                })
            try:
                checkpoint('network-guard')
                def route(item):
                    r = item.request
                    item.continue_() if guard.allow(r.method, r.url, r.post_data) else item.abort()
                context.route('**/*', route)
                def deny_socket(socket):
                    guard.denied = True
                    socket.close(code=1008, reason='closed browser recipe')
                context.route_web_socket('**/*', deny_socket)
                # Closing Firefox's last persistent page closes the browser.
                # Reuse its sole blank page; each viewport performs a fresh
                # navigation and its own real API request.
                if len(context.pages) != 1 or context.pages[0].url != 'about:blank':
                    raise ValueError('initial browsing scope')
                page = context.pages[0]
                for name, width, height in (('desktop', 1280, 900), ('mobile', 390, 844)):
                    checkpoint('page-start')
                    page.set_default_timeout(4000)
                    page.set_viewport_size({'width': width, 'height': height})
                    checkpoint('navigation')
                    response = page.goto(ORIGIN + '/', wait_until='domcontentloaded', timeout=5000)
                    if response is None or response.status != 200 or response.url != ORIGIN + '/':
                        raise ValueError('console navigation')
                    checkpoint('heading')
                    expect(page.get_by_role('heading', name='Verify workload access')).to_be_visible()
                    checkpoint('api-request')
                    with page.expect_response(lambda r: r.url == ORIGIN + '/api/demo'
                                              and r.request.method == 'POST', timeout=12000) as pending:
                        page.get_by_role('button', name='Run verification').click()
                    api = pending.value
                    checkpoint('api-response')
                    if api.status != 200 or api.headers.get('content-type') != 'application/json' or api.headers.get('set-cookie'):
                        raise ValueError('demo response headers')
                    response_hash = api_result(api.body())
                    checkpoint('interface-result')
                    expect(page.get_by_role('status')).to_contain_text('Verified.')
                    expect(page.locator('#algorithm')).to_have_text('AES-256-GCM')
                    expect(page.locator('#ttl')).to_have_text(expected)
                    checkpoint('layout')
                    if (len(page.frames) != 1 or len(context.pages) != 1 or context.cookies()
                            or not page.evaluate('document.documentElement.scrollWidth <= innerWidth')):
                        raise ValueError('console layout or browsing scope')
                    observations.append({'viewport': name, 'responseSHA256': response_hash})
                checkpoint('network-summary')
                if guard.denied or guard.posts != 2:
                    raise ValueError('network scope')
            finally:
                context.close()
    finished = int(time.time() * 1000)
    checkpoint('deadline')
    if not request['issuedAt'] <= started <= finished < request['expiresAt']:
        raise ValueError('browser deadline')
    return {'version': 'ai-sdlc.saferus-browser-result/v1', 'status': 'passed',
            'requestID': request['requestID'], 'requestSHA256': hashlib.sha256(request_raw).hexdigest(),
            'targetSHA256': request['targetSHA256'], 'snapshotSHA256': request['snapshotSHA256'],
            'imageDigest': request['imageDigest'], 'expectation': request['expectation'],
            'startedAt': started, 'finishedAt': finished, 'observations': observations,
            'normalTLS': True, 'networkScopePreserved': True}


def main():
    timer = None
    try:
        with Path('/input/request.json').open('rb') as stream:
            raw = stream.read(4097)
        request = parse_request(raw, int(time.time() * 1000))
        checkpoint('resource-limits')
        resource.setrlimit(resource.RLIMIT_NPROC, (256, 256))
        resource.setrlimit(resource.RLIMIT_NOFILE, (512, 512))
        resource.setrlimit(resource.RLIMIT_FSIZE, (16 * 1024 * 1024, 16 * 1024 * 1024))
        ca = Path('/input/ca.crt')
        checkpoint('approved-ca')
        with ca.open('rb') as stream:
            pem = stream.read(65537)
        if not 0 < len(pem) <= 65536 or pem.count(b'-----BEGIN CERTIFICATE-----') != 1:
            raise ValueError('approved CA')
        # PID 1 exits on the original finite budget even if a browser subprocess
        # or cleanup stops responding. Kubernetes also bounds the whole Job.
        timer = threading.Timer(min(25, request['expiresAt']/1000 - time.time()), lambda: os._exit(124))
        timer.daemon = True
        timer.start()
        result = run(request, raw, ca)
        output = json.dumps(result, separators=(',', ':'))
        if len(output.encode()) > 4096:
            raise ValueError('result bound')
        print(output, flush=True)
        return 0
    except Exception:
        print(json.dumps({'version': 'ai-sdlc.saferus-browser-result/v1', 'status': 'failed',
                          'reason': 'closed-browser-check-failed', 'boundary': BOUNDARY}), flush=True)
        return 1
    finally:
        if timer is not None:
            timer.cancel()


if __name__ == '__main__':
    sys.exit(main())
