#!/usr/bin/env node
// bin/zero-langfuse.js — thin shim that execs the platform-native binary placed
// next to the package by scripts/postinstall.mjs. If the binary is absent
// (ZLF_SKIP_DOWNLOAD, unsupported platform), point the user at the install docs.

import { spawn } from 'node:child_process';
import { existsSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const binary =
  process.platform === 'win32'
    ? join(here, '..', 'zero-langfuse.exe')
    : join(here, '..', 'zero-langfuse');

if (!existsSync(binary)) {
  console.error(
    '[zero-langfuse] no native binary installed; install via scripts/install.sh ' +
      'or build from source: https://github.com/nathanpt/zero-langfuse',
  );
  process.exit(127);
}

const child = spawn(binary, process.argv.slice(2), { stdio: 'inherit' });
child.on('close', (code) => process.exit(code ?? 0));
child.on('error', (err) => {
  console.error(`[zero-langfuse] failed to launch native binary: ${err.message}`);
  process.exit(1);
});
