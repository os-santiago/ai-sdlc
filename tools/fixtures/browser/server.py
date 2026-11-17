"""Synthetic UI contract fixture, never Saferus or cryptographic evidence."""
from http.server import BaseHTTPRequestHandler, HTTPServer
import json
import ssl
import sys

mode = sys.argv[1]
if mode not in ('baseline', 'readable', 'wrong-ttl', 'api-failure', 'extra-response', 'external-request'):
    raise ValueError('unknown fixture')
html = b'''<!doctype html><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Saferus synthetic contract fixture</title><h2>Verify workload access</h2>
<button id="run">Run verification</button><div role="status">Ready.</div>
<span id="algorithm"></span><span id="ttl"></span><script src="/static/app.js"></script>'''
script = '''document.querySelector('#run').onclick = async () => {
  EXTRA
  const response = await fetch('/api/demo', {method: 'POST', body: '{}'});
  const result = await response.json();
  document.querySelector('#algorithm').textContent = result.algorithm;
  document.querySelector('#ttl').textContent = TTL;
  document.querySelector('[role=status]').textContent = 'Verified.';
};'''.replace('TTL', json.dumps('1 minute' if mode == 'readable' else '59 seconds' if mode == 'wrong-ttl' else '60 seconds')).replace(
    'EXTRA', "try { await fetch('/forbidden'); } catch (_) {}" if mode == 'external-request' else '')


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def reply(self, status, kind, body):
        self.send_response(status)
        self.send_header('Content-Type', kind)
        self.send_header('Content-Length', str(len(body)))
        self.send_header('Cache-Control', 'no-store')
        self.send_header('Content-Security-Policy', "default-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'none'")
        self.send_header('X-Content-Type-Options', 'nosniff')
        self.send_header('Referrer-Policy', 'no-referrer')
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path == '/health/live':
            self.reply(204, 'text/plain', b'')
        elif self.path == '/':
            self.reply(200, 'text/html', html)
        elif self.path == '/static/app.js':
            self.reply(200, 'application/javascript', script.encode())
        else:
            self.reply(404, 'text/plain', b'')

    def do_POST(self):
        if self.path != '/api/demo' or self.headers.get('Content-Length') != '2' or self.rfile.read(2) != b'{}':
            self.reply(400, 'text/plain', b'')
            return
        response = {'verified': True, 'algorithm': 'AES-256-GCM', 'tokenTTLSeconds': 60,
                    'tokenRevoked': True, 'agentless': True}
        if mode == 'extra-response':
            response['unexpected'] = 'fixture-only'
        self.reply(500 if mode == 'api-failure' else 200, 'application/json', json.dumps(response).encode())


server = HTTPServer(('0.0.0.0', 8443), Handler)
server.timeout = 3
context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
context.minimum_version = ssl.TLSVersion.TLSv1_3
context.load_cert_chain('/tls/tls.crt', '/tls/tls.key')
server.socket = context.wrap_socket(server.socket, server_side=True)
server.serve_forever()
