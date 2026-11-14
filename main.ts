// Entry lifecycle for this forge: get a port, tell the trial env about it,
// then stay alive serving requests until the env reports the trial is over.
//
// The env lives at MAKEFUN_ENV_URL. This process never picks its own port
// number — it asks the kernel for a free one — and it never talks to port
// 8080 directly, because that port belongs to the env itself. Registration
// is a single POST carrying the chosen port; once the env has it, the suite
// can reach the forge. After that this process just waits, polling the env's
// /done endpoint, until the answer flips to true, and only then shuts the
// server down and leaves with a zero exit code.
//
// Anything that goes wrong in a way that cannot be recovered from — a
// missing env URL, or the env refusing registration outright — ends the
// process with exit code 1 and a line on stderr. Transient trouble, like
// the env not answering yet, is simply retried after a short pause that
// grows a little with each attempt.

import { createApp } from './app.ts';

// One request to the env is allowed this long before it is given up on.
const PER_REQUEST_MS = 30_000;
// Pauses between retries start here and never grow past the ceiling below.
const FIRST_PAUSE_MS = 250;
const LONGEST_PAUSE_MS = 2_000;

const rawEnvUrl = process.env.MAKEFUN_ENV_URL ?? '';
const envOrigin = rawEnvUrl.replace(/\/+$/, '');

if (envOrigin.length === 0) {
  process.stderr.write('entry: MAKEFUN_ENV_URL is empty, so there is no env to talk to\n');
  process.exit(1);
}

function pause(milliseconds: number): Promise<void> {
  return new Promise((done) => setTimeout(done, milliseconds));
}

// Pause length for the nth failed attempt: doubles each time from the
// starting pause, but stops growing once it hits the ceiling.
function pauseFor(attempt: number): number {
  let pauseMs = FIRST_PAUSE_MS;
  for (let step = 0; step < attempt && pauseMs < LONGEST_PAUSE_MS; step += 1) {
    pauseMs = Math.min(LONGEST_PAUSE_MS, pauseMs * 2);
  }
  return pauseMs;
}

// Tell the env which port the forge is listening on. A 200 means the env
// recorded it; a 409 means some entry already registered first, which is
// fine — this process still waits like any other. A 400 or 405 means the
// request itself was wrong, so there is no point retrying. Anything else —
// a dropped connection, a timeout, an unexpected status — is retried.
async function tellEnvOurPort(port: number): Promise<void> {
  let attempt = 0;
  for (;;) {
    try {
      const answer = await fetch(`${envOrigin}/register`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port }),
        signal: AbortSignal.timeout(PER_REQUEST_MS),
      });
      if (answer.status === 200 || answer.status === 409) {
        return;
      }
      if (answer.status === 400 || answer.status === 405) {
        throw new Error(`fatal: env refused registration with HTTP ${answer.status}`);
      }
      // Any other status: fall through to the retry pause below.
    } catch (problem) {
      if (problem instanceof Error && problem.message.startsWith('fatal:')) {
        throw problem;
      }
      // Network trouble or a timeout: the env may still be coming up.
    }
    await pause(pauseFor(attempt));
    attempt += 1;
  }
}

// Ask the env whether the trial has finished, over and over, until it says
// yes. A "no" answer, a bad reply, or a request that fails outright all
// just mean "not yet" — wait a little and ask again.
async function waitUntilEnvFinished(): Promise<void> {
  let attempt = 0;
  for (;;) {
    let finished = false;
    try {
      const answer = await fetch(`${envOrigin}/done`, {
        signal: AbortSignal.timeout(PER_REQUEST_MS),
      });
      if (answer.ok) {
        const payload = (await answer.json()) as { done?: unknown };
        finished = payload.done === true;
      }
    } catch {
      // The env did not answer in time, or not at all: treat as not done.
    }
    if (finished) {
      return;
    }
    await pause(pauseFor(attempt));
    attempt += 1;
  }
}

// Bring the forge up on a kernel-chosen port and hand back that port.
async function startForge(): Promise<{ server: ReturnType<typeof createApp>; port: number }> {
  const server = createApp();
  await new Promise<void>((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, () => resolve());
  });
  const bound = server.address();
  if (bound === null || typeof bound === 'string') {
    throw new Error('forge did not bind a TCP port');
  }
  return { server, port: bound.port };
}

async function run(): Promise<void> {
  const { server, port } = await startForge();
  process.stdout.write(`entry: forge listening on port ${port}\n`);

  await tellEnvOurPort(port);
  process.stdout.write('entry: registered with the env, waiting for it to finish\n');

  await waitUntilEnvFinished();

  await new Promise<void>((resolve) => server.close(() => resolve()));
  process.stdout.write('entry: env says done, shutting down\n');
}

try {
  await run();
  process.exit(0);
} catch (problem) {
  const detail = problem instanceof Error ? problem.message : String(problem);
  process.stderr.write(`entry: ${detail}\n`);
  process.exit(1);
}
