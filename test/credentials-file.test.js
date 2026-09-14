import assert from 'node:assert/strict'
import { mkdtemp, readFile, stat } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { describe, it } from 'node:test'

import { credentialRecord, writeCredentialsFile } from '../lib/credentials-file.js'

const tunnel = {
  endpoint: 'dsh-k3f9qz',
  remoteUrl: 'https://dsh-k3f9qz.clauded.friddle.me/',
  localPort: 43120,
  state: 'running',
  startedAt: '2026-09-14T06:00:00.000Z',
  expiresAt: '2026-09-14T10:00:00.000Z',
  authUser: 'k3f9qz',
  authPass: 's3cret-passphrase',
}

describe('credentialRecord', () => {
  it('keeps only what is needed to open the tunnel', () => {
    assert.deepEqual(credentialRecord(tunnel, () => '2026-09-14T06:00:01.000Z'), {
      endpoint: 'dsh-k3f9qz',
      remoteUrl: 'https://dsh-k3f9qz.clauded.friddle.me/',
      localPort: 43120,
      authUser: 'k3f9qz',
      authPass: 's3cret-passphrase',
      expiresAt: '2026-09-14T10:00:00.000Z',
      writtenAt: '2026-09-14T06:00:01.000Z',
    })
  })

  it('omits credentials and expiry for a tunnel that has none', () => {
    const record = credentialRecord({ endpoint: 'ep', remoteUrl: 'https://ep.example/', localPort: 80 })
    assert.equal('authUser' in record, false)
    assert.equal('authPass' in record, false)
    assert.equal('expiresAt' in record, false)
  })
})

describe('writeCredentialsFile', () => {
  it('writes the record with owner-only permissions', async () => {
    const dir = await mkdtemp(path.join(tmpdir(), 'piko-remote-test-'))
    const target = path.join(dir, 'nested', 'piko-access.json')

    await writeCredentialsFile(target, tunnel)

    const parsed = JSON.parse(await readFile(target, 'utf8'))
    assert.equal(parsed.remoteUrl, tunnel.remoteUrl)
    assert.equal(parsed.authPass, tunnel.authPass)

    const mode = (await stat(target)).mode & 0o777
    assert.equal(mode, 0o600, `expected 0600, got ${mode.toString(8)}`)
  })

  it('tightens permissions when rewriting an existing loose file', async () => {
    const dir = await mkdtemp(path.join(tmpdir(), 'piko-remote-test-'))
    const target = path.join(dir, 'access.json')

    await writeCredentialsFile(target, tunnel)
    const { chmod } = await import('node:fs/promises')
    await chmod(target, 0o644)
    await writeCredentialsFile(target, tunnel)

    const mode = (await stat(target)).mode & 0o777
    assert.equal(mode, 0o600)
  })
})
