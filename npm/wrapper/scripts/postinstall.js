'use strict';

// Runs after `npm install -g listo-cli`. It does NOTHING when npm installed the
// platform package (the normal case). Only when that package is missing
// (--omit=optional, a registry mirror without it) does it download this
// version's archive from the GitHub Release, check its sha256 against the
// release's checksums.txt, and unpack the binary into vendor/ for the shim.
//
// The checksum comes from the same release as the archive, so it catches a
// corrupt or truncated download but not a compromised release; the install
// script and "listo update" go further and verify a signature over
// checksums.txt. The common path (the platform package) is covered by npm's own
// integrity hashes and provenance.

const crypto = require('node:crypto');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { spawnSync } = require('node:child_process');

const { platformFor, archiveFor } = require('../lib/platform');
const { findBinary } = require('../lib/resolve');

const RELEASES = 'https://github.com/listo-delivery/listo-cli/releases';

async function download(fetchImpl, url) {
  const res = await fetchImpl(url, { redirect: 'follow' });
  if (!res.ok) throw new Error(`${url} answered ${res.status}`);
  return Buffer.from(await res.arrayBuffer());
}

// The sha256 checksums.txt lists for a file, or null.
function checksumFor(text, name) {
  for (const line of text.split('\n')) {
    const parts = line.trim().split(/\s+/);
    if (parts.length === 2 && parts[1].replace(/^\*/, '') === name) return parts[0].toLowerCase();
  }
  return null;
}

// bsdtar (macOS, Windows 10+) and GNU tar both unpack .tar.gz; bsdtar unpacks .zip too.
function extract(archivePath, intoDir) {
  const r = spawnSync('tar', ['-xf', archivePath, '-C', intoDir], { encoding: 'utf8' });
  if (r.error || r.status !== 0) {
    throw new Error(`could not unpack ${path.basename(archivePath)}: ${r.error ? r.error.message : r.stderr}`);
  }
}

// Downloads, verifies and installs the binary into vendorDir. Returns its path.
async function install({ version, info, vendorDir, base = RELEASES, fetchImpl = fetch }) {
  const archive = archiveFor(info);
  const dir = `${base}/download/v${version}`;
  const checksums = (await download(fetchImpl, `${dir}/checksums.txt`)).toString('utf8');
  const want = checksumFor(checksums, archive);
  if (!want) throw new Error(`${archive} is not listed in the release's checksums.txt`);

  const body = await download(fetchImpl, `${dir}/${archive}`);
  const got = crypto.createHash('sha256').update(body).digest('hex');
  if (got !== want) throw new Error(`the checksum of ${archive} does not match the release's checksums.txt`);

  const work = fs.mkdtempSync(path.join(os.tmpdir(), 'listo-cli-'));
  try {
    const archivePath = path.join(work, archive);
    fs.writeFileSync(archivePath, body);
    extract(archivePath, work);
    const unpacked = path.join(work, info.exe);
    if (!fs.existsSync(unpacked)) throw new Error(`${archive} holds no ${info.exe}`);
    fs.mkdirSync(vendorDir, { recursive: true });
    const target = path.join(vendorDir, info.exe);
    fs.copyFileSync(unpacked, target);
    fs.chmodSync(target, 0o755);
    return target;
  } finally {
    fs.rmSync(work, { recursive: true, force: true });
  }
}

async function main(opts = {}) {
  const platform = opts.platform || process.platform;
  const arch = opts.arch || process.arch;
  const log = opts.log || ((m) => process.stderr.write(`${m}\n`));
  const version = opts.version || require('../package.json').version;
  const vendorDir = opts.vendorDir || path.join(__dirname, '..', 'vendor');

  const found = findBinary({ platform, arch, vendorDir, resolvePkg: opts.resolvePkg });
  if (found.path) return 0; // the platform package is there: nothing to download

  const info = platformFor(platform, arch);
  if (!info) {
    log(`listo-cli: there is no build for ${platform}/${arch}, so nothing was installed.`);
    return 0; // do not fail the whole install of a project that lists listo-cli
  }

  try {
    log(`listo-cli: the ${info.pkg} package was not installed, downloading listo ${version} from GitHub.`);
    await install({ version, info, vendorDir, base: opts.base, fetchImpl: opts.fetchImpl });
    return 0;
  } catch (err) {
    log(`listo-cli: ${err.message}`);
    log('Install it another way:  curl -fsSL https://cli.listo.delivery/install.sh | sh');
    return 1;
  }
}

module.exports = { main, install, checksumFor };

if (require.main === module) {
  main().then((code) => {
    process.exitCode = code;
  });
}
