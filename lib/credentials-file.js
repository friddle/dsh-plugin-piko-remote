/**
 * Handing the access record to an operator.
 *
 * When the plugin exposes a port without a model in the loop (autoExpose on a
 * headless or remote box), the URL and the generated credentials exist only
 * inside the helper's stdout, which the supervisor consumes. Nobody is there to
 * read them. Logging them would be worse — DSH logs are files that outlive the
 * tunnel — so instead the record is written to a file the operator names, mode
 * 600, and the log only mentions the path.
 *
 * @module piko-remote/credentials-file
 */

import { chmod, mkdir, writeFile } from 'node:fs/promises'
import path from 'node:path'

/**
 * The record written for one tunnel.
 *
 * @param {object} tunnel - tunnel snapshot.
 * @param {() => string} [now] - clock, injectable for tests.
 * @returns {object} JSON-safe access record.
 */
export function credentialRecord(tunnel, now = () => new Date().toISOString()) {
  return {
    endpoint: tunnel.endpoint,
    remoteUrl: tunnel.remoteUrl,
    localPort: tunnel.localPort,
    ...(tunnel.authUser === undefined ? {} : { authUser: tunnel.authUser }),
    ...(tunnel.authPass === undefined ? {} : { authPass: tunnel.authPass }),
    ...(tunnel.expiresAt === undefined ? {} : { expiresAt: tunnel.expiresAt }),
    writtenAt: now(),
  }
}

/**
 * Write one tunnel's access record to `filePath` with owner-only permissions.
 *
 * `writeFile`'s mode only applies when the file is created, so `chmod` follows:
 * rewriting an existing world-readable file must not leave it world-readable.
 *
 * @param {string} filePath - destination path; parent directories are created.
 * @param {object} tunnel - tunnel snapshot.
 * @returns {Promise<string>} the path written.
 */
export async function writeCredentialsFile(filePath, tunnel) {
  const target = path.resolve(filePath)
  await mkdir(path.dirname(target), { recursive: true })
  await writeFile(target, `${JSON.stringify(credentialRecord(tunnel), null, 2)}\n`, { mode: 0o600 })
  await chmod(target, 0o600)
  return target
}
