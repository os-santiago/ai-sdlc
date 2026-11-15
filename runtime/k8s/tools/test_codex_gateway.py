import concurrent.futures
import json
import os
import threading
import unittest
import urllib.error
import urllib.request
from unittest.mock import patch

import codex_gateway as gateway
import codexcli_openai_wrapper as wrapper


class GatewayTests(unittest.TestCase):
    def test_authenticated_readiness_survives_busy_model_and_bounds_completion(self):
        server = gateway.GatewayServer(('127.0.0.1', 0), gateway.GatewayHandler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        entered, release = threading.Event(), threading.Event()
        token = 'g' * 48

        def complete(_):
            entered.set()
            release.wait(4)
            return {'role': 'assistant', 'content': 'fixture'}

        def call(path, authenticated=True, body=None):
            headers = {'Authorization': 'Bearer ' + token} if authenticated else {}
            if body is not None:
                headers['Content-Type'] = 'application/json'
                body = json.dumps(body).encode()
            request = urllib.request.Request(f'http://127.0.0.1:{server.server_port}' + path,
                                             data=body, headers=headers)
            with urllib.request.urlopen(request, timeout=3) as response:
                return json.load(response)

        with patch.dict(os.environ, CODEX_WRAPPER_TOKEN=token), patch.object(wrapper, 'complete', side_effect=complete):
            thread.start()
            try:
                with self.assertRaises(urllib.error.HTTPError) as denied:
                    call('/readyz', authenticated=False)
                self.assertEqual(denied.exception.code, 401)
                denied.exception.close()
                with concurrent.futures.ThreadPoolExecutor(max_workers=1) as pool:
                    first = pool.submit(call, '/v1/chat/completions', True, {'messages': []})
                    self.assertTrue(entered.wait(2))
                    try:
                        self.assertEqual(call('/readyz'), {'ready': True})
                        with self.assertRaises(urllib.error.HTTPError) as busy:
                            call('/v1/chat/completions', body={'messages': []})
                        self.assertEqual(busy.exception.code, 429)
                        busy.exception.close()
                    finally:
                        release.set()
                    self.assertEqual(first.result(2)['choices'][0]['message']['content'], 'fixture')
            finally:
                release.set()
                server.shutdown()
                server.server_close()
                thread.join()
