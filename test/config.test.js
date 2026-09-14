import assert from 'node:assert/strict'
import { describe, it } from 'node:test'

import { DEFAULTS, resolveConfig } from '../lib/config.js'

describe('resolveConfig', () => {
  it('returns the documented defaults for anything unset', () => {
    const config = resolveConfig()
    assert.equal(config.remote, DEFAULTS.remote)
    assert.equal(config.endpointPrefix, 'dsh')
    assert.equal(config.basicAuth, true)
    assert.equal(config.urlMode, 'subdomain')
    assert.equal(config.preserveHost, true)
    assert.equal(config.allowDshUiExpose, false)
    assert.equal(config.autoExpose, false)
    assert.equal(config.defaultTtlMinutes, 120)
  })

  it('keeps explicit values', () => {
    const config = resolveConfig({
      remote: 'https://piko.example',
      endpointPrefix: 'expose',
      defaultTtlMinutes: 15,
      basicAuth: false,
      urlMode: 'path',
      preserveHost: false,
      allowDshUiExpose: true,
      autoExpose: true,
      upstreamKey: 'key-123',
      helperPath: '/opt/piko-expose',
      connectTimeoutMs: 5000,
    })
    assert.equal(config.remote, 'https://piko.example')
    assert.equal(config.endpointPrefix, 'expose')
    assert.equal(config.defaultTtlMinutes, 15)
    assert.equal(config.basicAuth, false)
    assert.equal(config.urlMode, 'path')
    assert.equal(config.preserveHost, false)
    assert.equal(config.allowDshUiExpose, true)
    assert.equal(config.autoExpose, true)
    assert.equal(config.upstreamKey, 'key-123')
    assert.equal(config.helperPath, '/opt/piko-expose')
    assert.equal(config.connectTimeoutMs, 5000)
  })

  it('drops a routing mode the server cannot serve', () => {
    assert.equal(resolveConfig({ urlMode: 'banana' }).urlMode, 'subdomain')
  })

  it('treats the dangerous switches as opt-in only', () => {
    // Anything other than an explicit true must not enable them: these two
    // together publish this machine's agent to the internet.
    assert.equal(resolveConfig({ allowDshUiExpose: 'yes' }).allowDshUiExpose, false)
    assert.equal(resolveConfig({ autoExpose: 1 }).autoExpose, false)
    assert.equal(resolveConfig({ allowDshUiExpose: true }).allowDshUiExpose, true)
  })

  it('repairs nonsense numbers instead of failing later', () => {
    assert.equal(resolveConfig({ defaultTtlMinutes: -5 }).defaultTtlMinutes, 120)
    assert.equal(resolveConfig({ defaultTtlMinutes: 'soon' }).defaultTtlMinutes, 120)
    assert.equal(resolveConfig({ connectTimeoutMs: 0 }).connectTimeoutMs, 0)
  })

  it('falls back when the remote is emptied out', () => {
    assert.equal(resolveConfig({ remote: '' }).remote, DEFAULTS.remote)
  })

  it('returns a frozen object so later callers cannot mutate shared state', () => {
    assert.equal(Object.isFrozen(resolveConfig()), true)
  })
})
