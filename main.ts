// main.ts — thin launcher for the v9 Go forge. The forge binary is the
// server; this file only starts it, forwards signals, and exits with the
// child's code. It never deletes, moves, or rewrites any directory.
import { spawn, type ChildProcess } from 'node:child_process';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const dir = dirname(fileURLToPath(import.meta.url));
const forge = join(dir, 'go', 'forge');

const child: ChildProcess = spawn(forge, [], { stdio: 'inherit' });

let exiting = false;
const shutdown = (signal: NodeJS.Signals): void => {
  if (exiting) return;
  exiting = true;
  child.kill(signal);
};
process.once('SIGTERM', () => shutdown('SIGTERM'));
process.once('SIGINT', () => shutdown('SIGINT'));

child.once('error', (err) => {
  process.stderr.write(`[entry] failed to start the forge: ${err.message}\n`);
  process.exit(1);
});
child.once('exit', (code, signal) => {
  if (signal) process.kill(process.pid, signal);
  process.exit(code ?? 1);
});
