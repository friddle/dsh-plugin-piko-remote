import assert from 'node:assert/strict'
import { Readable } from 'node:stream'
import { describe, it } from 'node:test'

import { resolveConfig } from '../lib/config.js'
import {
  TunnelSupervisor,
  buildHelperArgv,
  createLineSplitter,
} from '../lib/supervisor.js'

/**
 * Build a spawner that hands the test a controllable child: a real Readable for
 * stdout, a promise for the exit outcome, and spies for termination.
 *
 * @returns {object} the spawner plus the specs and handles it produced.
 */
function fakeSpawn() {
  const specs = []
  const handles = []
  const spawn = (spec) => {
    const stdout = new Readable({ read() {} })
    let settle
    const done = new Promise((resolve) => {
      settle = resolve
    })
    const handle = {
      spec,
      stdout,
      stdin: undefined,
      stderr: undefined,
      collected: { stderr: { readFrom: () => ({ text: '', nextOffset: 0, lossy: false }) } },
      done,
      terminateCalls: 0,
      terminate() {
        this.terminateCalls += 1
      },
      async waitForExit() {
        settle({ exitCode: 0, signal: null })
        return true
      },
      /** Push one helper event line. */
      emit(payload) {
        stdout.push(`${JSON.stringify(payload)}\n`)
      },
      /** Push raw text, for split-line tests. */
      write(text) {
        stdout.push(text)
      },
      /** End the child without going through terminate(). */
      exit(outcome = { exitCode: 1, signal: null }) {
        settle(outcome)
      },
    }
    specs.push(spec)
    handles.push(handle)
    return handle
  }
  return { spawn, specs, handles }
}

/** Let queued stream events and microtasks run. */
const tick = () => new Promise((resolve) => setImmediate(resolve))

function makeSupervisor(overrides = {}, configOverrides = {}) {
  const { spawn, specs, handles } = fakeSpawn()
  const config = resolveConfig({
    remote: 'https://piko.test',
    endpointPrefix: 'dsh',
    defaultTtlMinutes: 60,
    basicAuth: true,
    ...configOverrides,
  })
  const supervisor = new TunnelSupervisor({
    spawn,
    config,
    defaultPort: 43120,
    helperPath: '/opt/piko-expose',
    readyTimeoutMs: 500,
    ...overrides,
  })
  return { supervisor, specs, handles }
}

describe('buildHelperArgv', () => {
  const base = {
    helperPath: '/opt/piko-expose',
    remote: 'https://clauded.friddle.me',
    endpoint: 'dsh-k3f9qz',
    port: 43120,
    urlMode: 'subdomain',
    basicAuth: true,
    preserveHost: true,
    upstreamKey: '',
    ttlMinutes: 60,
  }

  it('maps the tunnel parameters onto helper flags', () => {
    assert.deepEqual(buildHelperArgv(base), [
      '/opt/piko-expose',
      '--remote',
      'https://clauded.friddle.me',
      '--endpoint',
      'dsh-k3f9qz',
      '--target',
      '127.0.0.1:43120',
      '--url-mode',
      'subdomain',
      '--json',
      '--auto-exit',
      '60',
    ])
  })

  it('passes only the flags that differ from the helper defaults', () => {
    const argv = buildHelperArgv({ ...base, basicAuth: false, preserveHost: false, ttlMinutes: 0 })
    assert.ok(argv.includes('--auth=false'))
    assert.ok(argv.includes('--preserve-host=false'))
    assert.ok(!argv.includes('--auto-exit'), 'a zero TTL must not schedule an exit')
  })

  it('passes fixed Basic Auth credentials when configured', () => {
    const argv = buildHelperArgv({ ...base, authUser: 'friddle', authPass: 'sybran_20250807' })
    assert.equal(argv[argv.indexOf('--auth-user') + 1], 'friddle')
    assert.equal(argv[argv.indexOf('--auth-pass') + 1], 'sybran_20250807')
  })

  it('omits empty credentials so the helper keeps generating its own', () => {
    const argv = buildHelperArgv({ ...base, authUser: '', authPass: '' })
    assert.ok(!argv.includes('--auth-user'), 'an empty user must not become an empty flag')
    assert.ok(!argv.includes('--auth-pass'))
  })

  it('ignores fixed credentials when auth is disabled', () => {
    const argv = buildHelperArgv({ ...base, basicAuth: false, authUser: 'u', authPass: 'p' })
    assert.ok(!argv.includes('--auth-user'))
    assert.ok(!argv.includes('--auth-pass'))
  })

  it('forwards an upstream key only when one is set', () => {
    assert.ok(!buildHelperArgv(base).includes('--upstream-key'))
    const argv = buildHelperArgv({ ...base, upstreamKey: 'secret-key' })
    assert.equal(argv[argv.indexOf('--upstream-key') + 1], 'secret-key')
  })
})

describe('createLineSplitter', () => {
  it('reassembles events split across chunk boundaries', () => {
    const lines = []
    const feed = createLineSplitter((line) => lines.push(line))
    feed('{"event":"re')
    feed('ady","endpoint":"dsh-x"}')
    feed('\n{"event":"auth","user":"u"}')
    feed('\n')
    assert.deepEqual(lines, ['{"event":"ready","endpoint":"dsh-x"}', '{"event":"auth","user":"u"}'])
  })

  it('skips blank lines and keeps trailing partial data buffered', () => {
    const lines = []
    const feed = createLineSplitter((line) => lines.push(line))
    feed('\n\n{"a":1}\npartial')
    assert.deepEqual(lines, ['{"a":1}'])
    feed('\n')
    assert.deepEqual(lines, ['{"a":1}', 'partial'])
  })
})

describe('buildHelperArgv TTL mapping', () => {
  const base = {
    helperPath: '/opt/piko-expose',
    remote: 'https://piko.test',
    endpoint: 'dsh-x',
    port: 43120,
    urlMode: 'subdomain',
    basicAuth: true,
    ttlMinutes: 0,
  }

  it('omits --auto-exit when no TTL was asked for', () => {
    const argv = buildHelperArgv(base)
    assert.equal(argv.includes('--auto-exit'), false, 'a permanent tunnel must not carry a deadline')
  })

  it('passes --auto-exit only for a positive TTL', () => {
    const argv = buildHelperArgv({ ...base, ttlMinutes: 30 })
    const index = argv.indexOf('--auto-exit')
    assert.notEqual(index, -1)
    assert.equal(argv[index + 1], '30')
  })
})

describe('TunnelSupervisor.expose', () => {
  it('resolves once the helper reports ready, with credentials from the auth event', async () => {
    const { supervisor, specs, handles } = makeSupervisor()

    const pending = supervisor.expose({ port: 8080, name: 'dsh-x' })
    await tick()
    assert.equal(handles.length, 1)
    assert.equal(specs[0].argv[0], '/opt/piko-expose')
    assert.equal(specs[0].argv[specs[0].argv.indexOf('--endpoint') + 1], 'dsh-x')

    handles[0].emit({ event: 'auth', user: 'k3f9qz', pass: 's3cret-passphrase' })
    handles[0].emit({
      event: 'ready',
      endpoint: 'dsh-x',
      target: '127.0.0.1:8080',
      remoteUrl: 'https://dsh-x.piko.test/',
    })

    const tunnel = await pending
    assert.equal(tunnel.endpoint, 'dsh-x')
    assert.equal(tunnel.remoteUrl, 'https://dsh-x.piko.test/')
    assert.equal(tunnel.localPort, 8080)
    assert.equal(tunnel.state, 'running')
    assert.equal(tunnel.authUser, 'k3f9qz')
    assert.equal(tunnel.authPass, 's3cret-passphrase')
    assert.ok(tunnel.expiresAt !== undefined, 'a 60 minute TTL should produce an expiry')
  })

  it('honours the configured endpoint when the caller names none', async () => {
    // `--endpoint` reaches the plugin as config, and auto-expose never passes a
    // name: the configured value has to be the fallback, or a pinned URL is
    // impossible and every boot publishes a new one.
    const { supervisor, specs } = makeSupervisor({ readyTimeoutMs: 20 }, { endpoint: 'dsh-browser' })
    await assert.rejects(() => supervisor.expose({ port: 8080 }), /did not report ready within 20ms/)
    assert.equal(specs[0].argv[specs[0].argv.indexOf('--endpoint') + 1], 'dsh-browser')
  })

  it('lets an explicit name win over the configured endpoint', async () => {
    const { supervisor, specs } = makeSupervisor({ readyTimeoutMs: 20 }, { endpoint: 'dsh-browser' })
    await assert.rejects(() => supervisor.expose({ port: 8080, name: 'dsh-other' }), /did not report ready within 20ms/)
    assert.equal(specs[0].argv[specs[0].argv.indexOf('--endpoint') + 1], 'dsh-other')
  })

  it('defaults to the DSH web port when no port is given', async () => {
    const { supervisor, specs } = makeSupervisor()
    const pending = supervisor.expose()
    await tick()
    assert.equal(specs[0].argv[specs[0].argv.indexOf('--target') + 1], '127.0.0.1:43120')
    // Leave nothing running behind the assertion.
    await supervisor.dispose()
    await pending.catch(() => {})
  })

  it('accepts a lazy default port, since the web server may come up later', async () => {
    let port
    const { supervisor, specs } = makeSupervisor({ defaultPort: () => port })

    // Before the web server exists there is nothing to expose, and saying so
    // beats silently exposing the wrong port.
    await assert.rejects(() => supervisor.expose(), /no port to expose/)

    port = 43210
    const pending = supervisor.expose()
    await tick()
    assert.equal(specs[0].argv[specs[0].argv.indexOf('--target') + 1], '127.0.0.1:43210')
    await supervisor.dispose()
    await pending.catch(() => {})
  })

  it('fails clearly when there is no port to expose', async () => {
    const { supervisor } = makeSupervisor({ defaultPort: undefined })
    await assert.rejects(() => supervisor.expose(), /no port to expose/)
  })

  it('rejects an out-of-range port', async () => {
    const { supervisor } = makeSupervisor()
    await assert.rejects(() => supervisor.expose({ port: 70000 }), /invalid port/)
  })

  it('rejects when the helper reports an error', async () => {
    const { supervisor, handles } = makeSupervisor()
    const pending = supervisor.expose({ port: 8080 })
    await tick()
    handles[0].emit({ event: 'error', message: 'listen on endpoint "dsh-x": connection refused' })
    await assert.rejects(() => pending, /connection refused/)
    assert.equal(supervisor.list().length, 0, 'a failed tunnel must not stay listed')
  })

  it('rejects when the helper exits before it becomes ready', async () => {
    const { supervisor, handles } = makeSupervisor()
    const pending = supervisor.expose({ port: 8080 })
    await tick()
    handles[0].exit({ exitCode: 3, signal: null })
    await assert.rejects(() => pending, /exited \(code 3\)/)
  })

  it('times out, terminates the child and explains the failure', async () => {
    const { supervisor, handles } = makeSupervisor({ readyTimeoutMs: 20 })
    await assert.rejects(() => supervisor.expose({ port: 8080 }), /did not report ready within 20ms/)
    assert.equal(handles[0].terminateCalls, 1, 'the child must not be left running')
    assert.equal(supervisor.list().length, 0)
  })

  it('sanitises a requested endpoint name and returns the existing tunnel for a repeat', async () => {
    const { supervisor, handles } = makeSupervisor()
    const pending = supervisor.expose({ port: 8080, name: 'DSH_My App' })
    await tick()
    assert.equal(handles[0].spec.argv[handles[0].spec.argv.indexOf('--endpoint') + 1], 'dsh-my-app')
    handles[0].emit({ event: 'ready', endpoint: 'dsh-my-app', remoteUrl: 'https://dsh-my-app.piko.test/' })
    const first = await pending

    const second = await supervisor.expose({ port: 8080, name: 'dsh-my-app' })
    assert.deepEqual(second, first)
    assert.equal(handles.length, 1, 'a repeat request must not spawn a second helper')
  })

  it('uses configured fixed credentials for the tunnel', async () => {
    const { supervisor, specs } = makeSupervisor({}, { basicAuthUser: 'friddle', basicAuthPass: 'sybran_20250807' })
    const pending = supervisor.expose({ port: 8080 })
    await tick()
    assert.equal(specs[0].argv[specs[0].argv.indexOf('--auth-user') + 1], 'friddle')
    assert.equal(specs[0].argv[specs[0].argv.indexOf('--auth-pass') + 1], 'sybran_20250807')
    await supervisor.dispose()
    await pending.catch(() => {})
  })

  it('honours auth being turned off for one tunnel', async () => {
    const { supervisor, specs } = makeSupervisor()
    const pending = supervisor.expose({ port: 8080, auth: false })
    await tick()
    assert.ok(specs[0].argv.includes('--auth=false'))
    await supervisor.dispose()
    await pending.catch(() => {})
  })
})

describe('TunnelSupervisor.close / dispose', () => {
  it('closes one endpoint and leaves the others alone', async () => {
    const { supervisor, handles } = makeSupervisor()
    const first = supervisor.expose({ port: 8080, name: 'dsh-one' })
    const second = supervisor.expose({ port: 8081, name: 'dsh-two' })
    await tick()
    handles[0].emit({ event: 'ready', endpoint: 'dsh-one', remoteUrl: 'https://dsh-one.piko.test/' })
    handles[1].emit({ event: 'ready', endpoint: 'dsh-two', remoteUrl: 'https://dsh-two.piko.test/' })
    await Promise.all([first, second])
    assert.equal(supervisor.list().length, 2)

    assert.deepEqual(await supervisor.close('dsh-one'), ['dsh-one'])
    assert.equal(handles[0].terminateCalls, 1)
    assert.deepEqual(
      supervisor.list().map((tunnel) => tunnel.endpoint),
      ['dsh-two'],
    )
  })

  it('closes every tunnel when no endpoint is given', async () => {
    const { supervisor, handles } = makeSupervisor()
    const pending = supervisor.expose({ port: 8080, name: 'dsh-one' })
    await tick()
    handles[0].emit({ event: 'ready', endpoint: 'dsh-one', remoteUrl: 'https://dsh-one.piko.test/' })
    await pending

    assert.deepEqual(await supervisor.close(), ['dsh-one'])
    assert.equal(supervisor.list().length, 0)
  })

  it('disposing terminates children and refuses further exposes', async () => {
    const { supervisor, handles } = makeSupervisor()
    const pending = supervisor.expose({ port: 8080, name: 'dsh-one' })
    await tick()
    handles[0].emit({ event: 'ready', endpoint: 'dsh-one', remoteUrl: 'https://dsh-one.piko.test/' })
    await pending

    await supervisor.dispose()
    assert.equal(handles[0].terminateCalls, 1)
    await assert.rejects(() => supervisor.expose({ port: 8080 }), /shutting down/)
  })
})
