#!/usr/bin/env node

const { spawnSync } = require('child_process');
const path = require('path');

const BINARIES = {
  'linux-x64': 'zepp-life-linux-x64',
  'linux-arm64': 'zepp-life-linux-arm64',
  'darwin-x64': 'zepp-life-darwin-x64',
  'darwin-arm64': 'zepp-life-darwin-arm64',
  'win32-x64': 'zepp-life-win32-x64.exe',
  'win32-arm64': 'zepp-life-win32-arm64.exe',
};

const key = `${process.platform}-${process.arch}`;
const binary = BINARIES[key];

if (!binary) {
  const supported = Object.keys(BINARIES).join(', ');
  console.error(
    `zepp-life: unsupported platform "${key}". Supported platforms: ${supported}`
  );
  process.exit(1);
}

const result = spawnSync(
  path.join(__dirname, binary),
  process.argv.slice(2),
  { stdio: 'inherit' }
);

if (result.error) {
  console.error(`zepp-life: failed to run binary: ${result.error.message}`);
  process.exit(1);
}

process.exit(result.status === null ? 1 : result.status);
