'use strict';

const fs = require('node:fs');
const path = require('node:path');

const { platformFor } = require('./platform');

// Where the binary is, in the order the shim looks:
//
//   1. the platform package npm installed next to this one (the normal case);
//   2. vendor/<exe> inside this package, which postinstall.js fills ONLY when
//      the platform package is missing (npm run with --omit=optional, or an
//      unsupported-looking mirror).
//
// Everything the lookup touches is injected so a test can drive it offline.
function findBinary(opts = {}) {
  const platform = opts.platform || process.platform;
  const arch = opts.arch || process.arch;
  const resolvePkg = opts.resolvePkg || ((id) => require.resolve(id));
  const exists = opts.exists || fs.existsSync;
  const readJSON = opts.readJSON || ((p) => JSON.parse(fs.readFileSync(p, 'utf8')));
  const vendorDir = opts.vendorDir || path.join(__dirname, '..', 'vendor');

  const info = platformFor(platform, arch);
  if (!info) return { path: null, reason: 'unsupported', platform, arch };

  try {
    const manifest = resolvePkg(`${info.pkg}/package.json`);
    const binary = readJSON(manifest).listo.binary;
    const full = path.join(path.dirname(manifest), binary);
    if (exists(full)) return { path: full, source: 'package', info };
  } catch (_) {
    // Not installed: fall through to the downloaded copy.
  }

  const vendored = path.join(vendorDir, info.exe);
  if (exists(vendored)) return { path: vendored, source: 'vendor', info };

  return { path: null, reason: 'missing', platform, arch, info };
}

module.exports = { findBinary };
