'use strict';

// Offline tests for the wrapper's postinstall: it downloads only when the
// platform package is missing, and it verifies the archive's sha256 against the
// release's checksums.txt. A local HTTP server stands in for GitHub.

const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const fs = require('node:fs');
const http = require('node:http');
const os = require('node:os');
const path = require('node:path');
const { spawnSync } = require('node:child_process');
const { test } = require('node:test');

const { main, checksumFor } = require('../wrapper/scripts/postinstall');
const { platformFor, archiveFor } = require('../wrapper/lib/platform');

const VERSION = '9.8.7';
const info = platformFor('linux', 'x64');
const archive = archiveFor(info);

function tmp() {
  return fs.mkdtempSync(path.join(os.tmpdir(), 'listo-post-'));
}

// A real tar.gz holding a "listo" file, made by the system tar.
function makeArchive() {
  const dir = tmp();
  fs.writeFileSync(path.join(dir, 'listo'), '#!/bin/sh\necho hi\n');
  fs.writeFileSync(path.join(dir, 'README.md'), 'x');
  const out = path.join(tmp(), archive);
  const r = spawnSync('tar', ['-czf', out, '-C', dir, 'listo', 'README.md']);
  assert.equal(r.status, 0, String(r.stderr));
  return fs.readFileSync(out);
}

function serve(files) {
  const requests = [];
  const server = http.createServer((req, res) => {
    requests.push(req.url);
    const body = files[req.url];
    if (!body) {
      res.statusCode = 404;
      return res.end('nope');
    }
    res.end(body);
  });
  return new Promise((resolve) => {
    server.listen(0, '127.0.0.1', () => resolve({ base: `http://127.0.0.1:${server.address().port}`, requests, close: () => server.close() }));
  });
}

const sha = (buf) => crypto.createHash('sha256').update(buf).digest('hex');
const missingPackage = () => {
  throw new Error('MODULE_NOT_FOUND');
};
const noLog = { messages: [], fn(m) { this.messages.push(m); } };

function options(extra) {
  noLog.messages = [];
  return { platform: 'linux', arch: 'x64', version: VERSION, resolvePkg: missingPackage, log: (m) => noLog.fn(m), ...extra };
}

test('checksumFor reads sha256sum lines', () => {
  assert.equal(checksumFor('AAA  a.tar.gz\nbbb  *b.zip\n', 'a.tar.gz'), 'aaa');
  assert.equal(checksumFor('AAA  a.tar.gz\nbbb  *b.zip\n', 'b.zip'), 'bbb');
  assert.equal(checksumFor('AAA  a.tar.gz\n', 'c'), null);
});

test('with the platform package present it downloads nothing', async () => {
  const server = await serve({});
  try {
    const root = tmp();
    const pkgDir = path.join(root, info.pkg);
    fs.mkdirSync(path.join(pkgDir, 'bin'), { recursive: true });
    fs.writeFileSync(path.join(pkgDir, 'package.json'), JSON.stringify({ listo: { binary: 'bin/listo' } }));
    fs.writeFileSync(path.join(pkgDir, 'bin', 'listo'), 'x');
    const code = await main(options({ base: server.base, vendorDir: path.join(root, 'vendor'), resolvePkg: () => path.join(pkgDir, 'package.json') }));
    assert.equal(code, 0);
    assert.deepEqual(server.requests, []);
    assert.equal(fs.existsSync(path.join(root, 'vendor')), false);
  } finally {
    server.close();
  }
});

test('with the package missing it downloads, verifies the checksum and installs into vendor/', async () => {
  const body = makeArchive();
  const server = await serve({
    [`/download/v${VERSION}/checksums.txt`]: `${sha(body)}  ${archive}\n${'0'.repeat(64)}  other.zip\n`,
    [`/download/v${VERSION}/${archive}`]: body,
  });
  try {
    const vendorDir = path.join(tmp(), 'vendor');
    const code = await main(options({ base: server.base, vendorDir }));
    assert.equal(code, 0, noLog.messages.join('\n'));
    const target = path.join(vendorDir, 'listo');
    assert.equal(fs.readFileSync(target, 'utf8'), '#!/bin/sh\necho hi\n');
    if (process.platform !== 'win32') assert.ok(fs.statSync(target).mode & 0o111, 'the binary must be executable');
    assert.deepEqual(server.requests.sort(), [`/download/v${VERSION}/${archive}`, `/download/v${VERSION}/checksums.txt`].sort());
  } finally {
    server.close();
  }
});

test('a tampered archive is refused and nothing is installed', async () => {
  const body = makeArchive();
  const tampered = Buffer.concat([body, Buffer.from('x')]);
  const server = await serve({
    [`/download/v${VERSION}/checksums.txt`]: `${sha(body)}  ${archive}\n`,
    [`/download/v${VERSION}/${archive}`]: tampered,
  });
  try {
    const vendorDir = path.join(tmp(), 'vendor');
    const code = await main(options({ base: server.base, vendorDir }));
    assert.equal(code, 1);
    assert.match(noLog.messages.join('\n'), /checksum .* does not match/);
    assert.match(noLog.messages.join('\n'), /install\.sh/);
    assert.equal(fs.existsSync(vendorDir), false);
  } finally {
    server.close();
  }
});

test('an archive missing from checksums.txt, or a 404, fails with the next step', async () => {
  const body = makeArchive();
  const unlisted = await serve({
    [`/download/v${VERSION}/checksums.txt`]: `${sha(body)}  something-else.tar.gz\n`,
    [`/download/v${VERSION}/${archive}`]: body,
  });
  try {
    assert.equal(await main(options({ base: unlisted.base, vendorDir: path.join(tmp(), 'v') })), 1);
    assert.match(noLog.messages.join('\n'), /not listed/);
  } finally {
    unlisted.close();
  }
  const empty = await serve({});
  try {
    assert.equal(await main(options({ base: empty.base, vendorDir: path.join(tmp(), 'v') })), 1);
    assert.match(noLog.messages.join('\n'), /404/);
  } finally {
    empty.close();
  }
});

test('an unsupported platform does not fail the install', async () => {
  const code = await main(options({ platform: 'sunos', arch: 'x64', vendorDir: path.join(tmp(), 'v') }));
  assert.equal(code, 0);
  assert.match(noLog.messages.join('\n'), /no build for sunos\/x64/);
});
