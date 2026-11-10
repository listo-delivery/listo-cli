#!/usr/bin/env node
'use strict';

// The `listo` command when listo-cli was installed with npm: it finds the real
// binary (see lib/resolve.js), runs it with the same arguments and streams, and
// exits the way it exited.
//
// LISTO_INSTALL_METHOD=npm is how the binary knows an npm install owns it, so
// its once-a-day update notice names "npm i -g listo-cli" and "listo update"
// points at npm instead of replacing a file npm manages.

const { spawnSync } = require('node:child_process');

const { findBinary } = require('../lib/resolve');

function run(argv, env, opts = {}) {
  const found = findBinary(opts);
  if (!found.path) {
    process.stderr.write(missingMessage(found));
    return 1;
  }
  const child = spawnSync(found.path, argv, {
    stdio: 'inherit',
    env: { ...env, LISTO_INSTALL_METHOD: 'npm' },
  });
  if (child.error) {
    process.stderr.write(`listo: could not run ${found.path}: ${child.error.message}\n`);
    return 1;
  }
  if (child.signal) {
    // Die by the same signal, so a shell sees Ctrl-C as Ctrl-C.
    process.kill(process.pid, child.signal);
    return 1;
  }
  return child.status === null ? 1 : child.status;
}

function missingMessage(found) {
  if (found.reason === 'unsupported') {
    return (
      `listo: there is no build for ${found.platform}/${found.arch}.\n` +
      'Supported: macOS and Linux on x64 and arm64, Windows on x64.\n'
    );
  }
  return (
    `listo: the program for ${found.platform}/${found.arch} was not installed.\n` +
    'Reinstall without --ignore-scripts and --omit=optional:  npm i -g listo-cli\n' +
    'Or install it another way:  curl -fsSL https://cli.listo.delivery/install.sh | sh\n'
  );
}

module.exports = { run, missingMessage };

if (require.main === module) {
  process.exitCode = run(process.argv.slice(2), process.env);
}
