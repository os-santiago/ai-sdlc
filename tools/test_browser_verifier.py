"""Reject expanded browser authority, false demo results and external traffic."""
import importlib.util
import json
from pathlib import Path
import unittest

path = Path(__file__).resolve().parents[1] / 'runtime/k8s/components/browser-verifier/browser.py'
spec = importlib.util.spec_from_file_location('closed_browser', path)
browser = importlib.util.module_from_spec(spec)
spec.loader.exec_module(browser)


class BrowserContract(unittest.TestCase):
    def request(self):
        return {'version': 'ai-sdlc.saferus-browser-request/v1', 'requestID': 'browser-'+'a'*64,
                'targetSHA256': 'b'*64, 'snapshotSHA256': 'c'*64, 'imageDigest': 'sha256:'+'d'*64,
                'issuedAt': 100000, 'expiresAt': 125000, 'expectation': 'baseline-seconds'}

    def test_scope_deadline_and_duplicate_fields_cannot_expand(self):
        original = self.request()
        self.assertEqual(browser.parse_request(json.dumps(original).encode(), 101000), original)
        for key, value in (('url', 'https://example.com'), ('script', 'arbitrary'),
                           ('expiresAt', 131000), ('expiresAt', 102000), ('issuedAt', True),
                           ('issuedAt', 120000), ('imageDigest', 'latest'), ('requestID', 'arbitrary'),
                           ('expectation', 'arbitrary'), ('snapshotSHA256', '')):
            raw = json.dumps(dict(original, **{key: value})).encode()
            with self.subTest(key=key, value=value), self.assertRaises(ValueError):
                browser.parse_request(raw, 101000)
        raw = json.dumps(original).encode()
        for invalid in (b'', raw[:-1]+b',"expiresAt":125000}', raw+b' '*4096):
            with self.assertRaises(ValueError): browser.parse_request(invalid, 101000)

    def test_browser_cannot_send_external_or_unapproved_requests(self):
        good = browser.NetworkGuard()
        self.assertTrue(good.allow('GET', browser.ORIGIN+'/', None))
        self.assertTrue(good.allow('POST', browser.ORIGIN+'/api/demo', '{}'))
        self.assertTrue(good.allow('POST', browser.ORIGIN+'/api/demo', '{}'))
        self.assertFalse(good.allow('POST', browser.ORIGIN+'/api/demo', '{}'))
        self.assertTrue(good.denied)
        for method, url, body in (
            ('GET', 'https://example.com/', None),
            ('GET', 'http://saferus-console:8443/', None),
            ('GET', 'https://saferus-console.attacker:8443/', None),
            ('GET', browser.ORIGIN+'/static/../api/demo', None),
            ('GET', browser.ORIGIN+'/?data=unexpected', None),
            ('GET', browser.ORIGIN+'/v1/sys/health', None),
            ('POST', browser.ORIGIN+'/api/demo', '{"input":"unexpected"}'),
            ('DELETE', browser.ORIGIN+'/api/demo', None),
        ):
            with self.subTest(url=url):
                guard = browser.NetworkGuard()
                self.assertFalse(guard.allow(method, url, body))
                self.assertTrue(guard.denied)
        guard = browser.NetworkGuard()
        self.assertTrue(all(guard.allow('GET', browser.ORIGIN+'/', None) for _ in range(16)))
        self.assertFalse(guard.allow('GET', browser.ORIGIN+'/', None))

    def test_demo_must_report_real_expected_success_without_extra_data(self):
        original = {'verified': True, 'algorithm': 'AES-256-GCM', 'tokenTTLSeconds': 60,
                    'tokenRevoked': True, 'agentless': True}
        raw = json.dumps(original).encode()
        self.assertEqual(len(browser.api_result(raw)), 64)
        for key, value in (('verified', False), ('tokenRevoked', False), ('agentless', False),
                           ('algorithm', 'unknown'), ('tokenTTLSeconds', True),
                           ('tokenTTLSeconds', 61), ('tokenTTLSeconds', 59), ('token', 'unexpected')):
            with self.subTest(key=key), self.assertRaises(ValueError):
                browser.api_result(json.dumps(dict(original, **{key: value})).encode())
        with self.assertRaises(ValueError): browser.api_result(raw[:-1]+b',"verified":true}')


if __name__ == '__main__':
    unittest.main()
