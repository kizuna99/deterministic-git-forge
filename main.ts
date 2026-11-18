// Process lifecycle for the forge entry, written for this submission.
//
// The grading env owns the trial: it tells the entry where it lives through
// MAKEFUN_ENV_URL, the entry answers with the ephemeral port its HTTP server
// bound, and then the entry simply stays alive until the env reports the
// trial finished. Only then may the process exit, with code 0.
//
// Contract, step by step:
//   - No MAKEFUN_ENV_URL: nothing to register with, exit 1 immediately.
//   - The server binds port 0 (kernel-assigned). Port 8080 belongs to the env
//     and is never bound here.
//   - POST {env}/register with {"port": N}. A 200 records this entry; a 409
//     means a first registration already won and is equally final. A 400 or
//     405 is a contract error and aborts the process. Anything else (the env
//     may still be booting) is retried with capped exponential backoff.
//   - GET {env}/done is polled; the env answers {"done": false} while the
//     suite runs and {"done": true} once it is over. Failed polls are just
//     "not done yet" and are retried the same way.
//   - On done: close the server and exit 0 so the harness can collect the
//     trial export.

import { createApp } from './app.ts';

const envOrigin = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');
if (envOrigin === '') {
  process.stderr.write('[forge] MAKEFUN_ENV_URL missing: no env to register with, exiting\n');
  process.exit(1);
}

const CALL_TIMEOUT_MS = 30_000;
const BACKOFF_FIRST_MS = 250;
const BACKOFF_CAP_MS = 2_000;

function pause(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/** Delay before retry number `attempt`: 250ms doubling, capped at 2s. */
function backoffMs(attempt: number): number {
  return Math.min(BACKOFF_CAP_MS, BACKOFF_FIRST_MS * 2 ** Math.min(attempt, 8));
}

/** Tell the env which port the server bound; retries until accepted. */
async function announcePort(port: number): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${envOrigin}/register`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port }),
        signal: AbortSignal.timeout(CALL_TIMEOUT_MS),
      });
      if (res.status === 200 || res.status === 409) return;
      if (res.status === 400 || res.status === 405) {
        throw new Error(`register refused with HTTP ${res.status}: ${await res.text()}`);
      }
    } catch (err) {
      if (err instanceof Error && err.message.startsWith('register refused')) throw err;
    }
    await pause(backoffMs(attempt));
  }
}

/** Block until the env's /done endpoint answers {"done": true}. */
async function awaitTrialEnd(): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${envOrigin}/done`, { signal: AbortSignal.timeout(CALL_TIMEOUT_MS) });
      if (res.ok) {
        const body = (await res.json()) as { done?: unknown };
        if (body.done === true) return;
      }
    } catch {
      // Env unreachable or the poll timed out: the trial is not over, poll again.
    }
    await pause(backoffMs(attempt));
  }
}

async function run(): Promise<void> {
  const server = createApp();
  await new Promise<void>((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, () => resolve());
  });
  const address = server.address();
  if (address === null || typeof address === 'string') {
    throw new Error('server did not bind an ephemeral TCP port');
  }
  process.stdout.write(`[forge] http server listening on port ${address.port}\n`);

  await announcePort(address.port);
  process.stdout.write('[forge] registered with the env; waiting for the trial to end\n');

  await awaitTrialEnd();
  server.close();
  process.stdout.write('[forge] trial over; shutting down\n');
}

try {
  await run();
  process.exit(0);
} catch (err) {
  process.stderr.write(`[forge] ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
}
