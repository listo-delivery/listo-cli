'use strict';

// The five platforms listo ships a binary for, keyed the way Node names them
// (process.platform-process.arch). Each has its own npm package holding only that
// platform's binary, and the wrapper lists all five as optionalDependencies so
// npm installs just the one that matches the computer.
//
// The package names here, the optionalDependencies in package.json and the
// manifests under npm/platforms must agree: a test holds them equal.

const PLATFORMS = {
  'darwin-arm64': { pkg: 'listo-cli-darwin-arm64', goos: 'darwin', goarch: 'arm64', exe: 'listo' },
  'darwin-x64': { pkg: 'listo-cli-darwin-x64', goos: 'darwin', goarch: 'amd64', exe: 'listo' },
  'linux-arm64': { pkg: 'listo-cli-linux-arm64', goos: 'linux', goarch: 'arm64', exe: 'listo' },
  'linux-x64': { pkg: 'listo-cli-linux-x64', goos: 'linux', goarch: 'amd64', exe: 'listo' },
  'win32-x64': { pkg: 'listo-cli-win32-x64', goos: 'windows', goarch: 'amd64', exe: 'listo.exe' },
};

function platformFor(platform, arch) {
  return PLATFORMS[`${platform}-${arch}`] || null;
}

// The release archive for a platform: the names GoReleaser writes
// (listo_<goos>_<goarch>.tar.gz, and .zip on Windows).
function archiveFor(info) {
  return `listo_${info.goos}_${info.goarch}.${info.goos === 'windows' ? 'zip' : 'tar.gz'}`;
}

module.exports = { PLATFORMS, platformFor, archiveFor };
