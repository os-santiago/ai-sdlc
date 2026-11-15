// Trusted conformance fixture, never an issue-to-work entrypoint.
'use strict';
const fs = require('fs');
const http = require('http');
const net = require('net');
const dgram = require('dgram');
const os = require('os');

const mode = process.argv[1];
const target = process.argv[2];
const marker = 'ai-sdlc-offline-probe-v1';

function rootMountReadOnly(mountinfo) {
  const roots = mountinfo.trim().split('\n').map(line => line.split(' ')).filter(fields => fields[4] === '/');
  return roots.length > 0 && roots.every(fields => fields[5].split(',').includes('ro'));
}

async function tcp(host, port = 8080) {
  return new Promise((resolve, reject) => {
    const request = http.get({ host, port, path: '/', timeout: 1500 }, response => {
      let body = '';
      response.on('data', data => { body += data; if (body.length > 128) request.destroy(); });
      response.on('end', () => body === marker ? resolve('reachable') : reject(Error('unexpected response')));
    });
    request.on('timeout', () => request.destroy(Error('probe timeout')));
    request.on('error', error => {
      if (['ENETUNREACH', 'EHOSTUNREACH'].includes(error.code)) resolve('unreachable');
      else reject(error);
    });
  });
}

async function udp(host, family) {
  return new Promise((resolve, reject) => {
    const socket = dgram.createSocket(family);
    const timer = setTimeout(() => finish(Error('probe timeout')), 1500);
    let done = false;
    function finish(error, value) {
      if (done) return;
      done = true;
      clearTimeout(timer);
      socket.close();
      if (error && !['ENETUNREACH', 'EHOSTUNREACH'].includes(error.code)) reject(error);
      else resolve(error ? 'unreachable' : value);
    }
    socket.on('error', error => finish(error));
    socket.on('message', body => finish(body.toString() === marker ? null : Error('unexpected response'), 'reachable'));
    socket.send(marker, 8080, host, error => { if (error) finish(error); });
  });
}

async function main() {
  if (mode === 'serve') {
    http.createServer((_request, response) => response.end(marker)).listen(8080, '::');
    const socket = dgram.createSocket({ type: 'udp6', ipv6Only: false });
    socket.on('message', (_body, peer) => socket.send(marker, peer.port, peer.address));
    socket.bind(8080, '::');
    return;
  }
  if (mode === 'network') {
    if (!net.isIPv4(target)) throw Error('expected IPv4 probe destination');
    const results = {};
    results.tcp4 = await tcp(target);
    results.tcp6Mapped = await tcp('::ffff:' + target);
    results.udp4 = await udp(target, 'udp4');
    results.udp6Mapped = await udp('::ffff:' + target, 'udp6');
    console.log(JSON.stringify(results));
    return;
  }
  if (mode !== 'local') throw Error('unknown conformance mode');
  if (!fs.readFileSync('/proc/version', 'utf8').includes('gvisor')) throw Error('gVisor not observed');
  if (Object.values(os.networkInterfaces()).flat().some(address => !address.internal)) throw Error('external interface present');
  if (!rootMountReadOnly(fs.readFileSync('/proc/self/mountinfo', 'utf8'))) throw Error('root mount is not read-only');
  if (process.getuid() === 0 || !/^CapEff:\s+0+$/m.test(fs.readFileSync('/proc/self/status', 'utf8'))) throw Error('privileged probe');
  for (const path of ['/var/run/secrets/kubernetes.io/serviceaccount/token', '/var/run/docker.sock',
    '/run/containerd/containerd.sock', '/secrets', '/root/.codex/auth.json']) {
    if (fs.existsSync(path)) throw Error('unexpected credential or socket mount');
  }
  let readOnly = false;
  try { fs.writeFileSync('/forbidden-probe', marker); }
  catch (error) { if (['EROFS', 'EACCES'].includes(error.code)) readOnly = true; else throw error; }
  if (!readOnly) throw Error('root filesystem writable');
  fs.writeFileSync('/workspaces/positive-probe', marker);
  if (fs.readFileSync('/workspaces/positive-probe', 'utf8') !== marker) throw Error('workspace unavailable');
  fs.unlinkSync('/workspaces/positive-probe');
  for (const host of ['127.0.0.1', '::1']) {
    const server = http.createServer((_request, response) => response.end(marker));
    await new Promise((resolve, reject) => { server.on('error', reject); server.listen(0, host, resolve); });
    if (await tcp(host, server.address().port) !== 'reachable') throw Error('loopback unavailable');
    await new Promise(resolve => server.close(resolve));
  }
  console.log(JSON.stringify({ gvisor: true, loopback4: true, loopback6: true,
    nonRoot: true, capabilitiesDropped: true, credentialMountsAbsent: true,
    rootReadOnly: true, workspaceWritable: true, externalInterfacesAbsent: true }));
}
function run() {
  main().catch(() => { console.error('offline conformance fixture failed'); process.exit(2); });
}
module.exports = { rootMountReadOnly, run };
if (require.main === module) run();
