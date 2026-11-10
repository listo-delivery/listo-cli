'use strict';

// Offline tests for the wrapper: how the shim finds the binary and how it runs it.
// Run with plain Node:  node --test "cli/npm/test/*-test.js"

const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { test } = require('node:test');

const { findBinary } = require('../wrapper/lib/resolve');
const { PLATFORMS, platformFor, archiveFor } = require('../wrapper/lib/platform');
const { run, missingMessage } = require('../wrapper/bin/shim');

const wrapper = JSON.parse(fs.readFileSync(path.join(__dirname, '../wrapper/package.json'), 'utf8'));

function tmp() {
  return fs.mkdtempSync(path.join(os.tmpdir(), 'listo-shim-'));
}

// A fake installed platform package with a binary that prints what it was given.
function fakePlatformPackage(root, info, script) {
  const dir = path.join(root, 'node_modules', info.pkg);
  fs.mkdirSync(path.join(dir, 'bin'), { recursive: true });
  fs.writeFileSync(path.join(dir, 'package.json'), JSON.stringify({ name: info.pkg, listo: { binary: `bin/${info.exe}` } }));
  const bin = path.join(dir, 'bin', info.exe);
  fs.writeFileSync(bin, script, { mode: 0o755 });
  return { dir, bin, manifest: path.join(dir, 'package.json') };
}

test('the five platforms, their package names and the wrapper agree', () => {
  assert.deepEqual(Object.keys(PLATFORMS).sort(), ['darwin-arm64', 'darwin-x64', 'linux-arm64', 'linux-x64', 'win32-x64']);
  assert.deepEqual(
    Object.keys(wrapper.optionalDependencies).sort(),
    Object.values(PLATFORMS).map((p) => p.pkg).sort(),
  );
  assert.equal(wrapper.name, 'listo-cli');
  assert.deepEqual(wrapper.bin, { listo: 'bin/shim.js' });
  assert.equal(wrapper.scripts.postinstall, 'node scripts/postinstall.js');
  for (const [key, info] of Object.entries(PLATFORMS)) {
    assert.equal(info.pkg, `listo-cli-${key}`);
    assert.equal(wrapper.optionalDependencies[info.pkg], wrapper.version, `${info.pkg} must be pinned to the wrapper's version`);
  }
});

test('every platform manifest names its os, cpu and binary, and carries the wrapper version', () => {
  for (const [key, info] of Object.entries(PLATFORMS)) {
    const manifest = JSON.parse(fs.readFileSync(path.join(__dirname, '../platforms', key, 'package.json'), 'utf8'));
    const [platform, arch] = key.split('-');
    assert.equal(manifest.name, info.pkg);
    assert.equal(manifest.version, wrapper.version);
    assert.deepEqual(manifest.os, [platform]);
    assert.deepEqual(manifest.cpu, [arch]);
    assert.equal(manifest.listo.binary, `bin/${info.exe}`);
    assert.equal(manifest.bin, undefined, 'a platform package must not link a listo command of its own');
    assert.equal(manifest.repository.url, wrapper.repository.url, 'provenance checks the repository');
  }
});

test('the release archive names are the ones the release builds', () => {
  assert.equal(archiveFor(platformFor('linux', 'x64')), 'listo_linux_amd64.tar.gz');
  assert.equal(archiveFor(platformFor('darwin', 'arm64')), 'listo_darwin_arm64.tar.gz');
  assert.equal(archiveFor(platformFor('win32', 'x64')), 'listo_windows_amd64.zip');
  assert.equal(platformFor('freebsd', 'x64'), null);
  assert.equal(platformFor('win32', 'arm64'), null);
});

test('findBinary prefers the platform package, then vendor/, then says why it found nothing', () => {
  const root = tmp();
  const info = platformFor('linux', 'x64');
  const vendorDir = path.join(root, 'vendor');
  const resolveIn = (r) => (id) => {
    const p = path.join(r, 'node_modules', id);
    if (!fs.existsSync(p)) throw new Error('MODULE_NOT_FOUND');
    return p;
  };

  assert.deepEqual(
    { reason: findBinary({ platform: 'linux', arch: 'x64', vendorDir, resolvePkg: resolveIn(root) }).reason },
    { reason: 'missing' },
  );

  fs.mkdirSync(vendorDir);
  fs.writeFileSync(path.join(vendorDir, 'listo'), '#!/bin/sh\n');
  const vendored = findBinary({ platform: 'linux', arch: 'x64', vendorDir, resolvePkg: resolveIn(root) });
  assert.equal(vendored.source, 'vendor');
  assert.equal(vendored.path, path.join(vendorDir, 'listo'));

  const pkg = fakePlatformPackage(root, info, '#!/bin/sh\n');
  const packaged = findBinary({ platform: 'linux', arch: 'x64', vendorDir, resolvePkg: resolveIn(root) });
  assert.equal(packaged.source, 'package');
  assert.equal(packaged.path, pkg.bin);

  assert.equal(findBinary({ platform: 'sunos', arch: 'x64' }).reason, 'unsupported');
});

test('the shim passes arguments through, sets LISTO_INSTALL_METHOD=npm and returns the exit status', { skip: process.platform === 'win32' }, () => {
  const root = tmp();
  const info = platformFor(process.platform, process.arch) || platformFor('linux', 'x64');
  const out = path.join(root, 'seen.txt');
  fakePlatformPackage(root, info, `#!/bin/sh\nprintf '%s|%s' "$LISTO_INSTALL_METHOD" "$*" > '${out}'\nexit 7\n`);

  const resolvePkg = (id) => path.join(root, 'node_modules', id);
  const status = run(['login', '--live', 'a b'], { PATH: process.env.PATH, LISTO_INSTALL_METHOD: 'script' }, {
    platform: info.goos === 'darwin' ? 'darwin' : 'linux',
    arch: info.goarch === 'arm64' ? 'arm64' : 'x64',
    resolvePkg,
    vendorDir: path.join(root, 'none'),
  });

  assert.equal(status, 7);
  assert.equal(fs.readFileSync(out, 'utf8'), 'npm|login --live a b', 'npm wins over a value already in the environment');
});

test('with no binary the shim exits 1 and names the way out, with no provider and no crash', () => {
  const written = [];
  const real = process.stderr.write;
  process.stderr.write = (s) => {
    written.push(String(s));
    return true;
  };
  let status;
  try {
    status = run([], {}, { platform: 'linux', arch: 'x64', resolvePkg: () => { throw new Error('nope'); }, vendorDir: path.join(tmp(), 'none') });
  } finally {
    process.stderr.write = real;
  }
  assert.equal(status, 1);
  const text = written.join('');
  assert.match(text, /npm i -g listo-cli/);
  assert.match(text, /install\.sh/);
  assert.match(missingMessage({ reason: 'unsupported', platform: 'sunos', arch: 'x64' }), /no build for sunos\/x64/);
});
