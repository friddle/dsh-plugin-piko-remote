import assert from 'node:assert/strict'
import { describe, it } from 'node:test'

import {
  ENDPOINT_PATTERN,
  MAX_ENDPOINT_LENGTH,
  isValidEndpoint,
  randomEndpoint,
  remoteUrlFor,
  sanitizeEndpoint,
} from '../lib/endpoint.js'

describe('isValidEndpoint', () => {
  it('accepts DNS-label shaped endpoints', () => {
    for (const value of ['dsh', 'dsh-a1b2c3', 'a', 'a-b', 'x'.repeat(63), '9lives']) {
      assert.equal(isValidEndpoint(value), true, `${value} should be valid`)
    }
  })

  it('rejects anything the public server would refuse', () => {
    for (const value of [
      '',
      'Dsh',
      'dsh_a1b2c3',
      '-leading',
      'trailing-',
      'has space',
      'dot.name',
      'emoji-🙂',
      'x'.repeat(64),
      undefined,
      null,
      42,
    ]) {
      assert.equal(isValidEndpoint(value), false, `${String(value)} should be invalid`)
    }
  })

  it('agrees with the published pattern', () => {
    const pattern = new RegExp(ENDPOINT_PATTERN)
    assert.equal(pattern.test('dsh-a1b2c3'), true)
    assert.equal(pattern.test('-dsh'), false)
  })
})

describe('sanitizeEndpoint', () => {
  it('lowercases and replaces illegal runs with single dashes', () => {
    assert.equal(sanitizeEndpoint('DSH_A1B2C3'), 'dsh-a1b2c3')
    assert.equal(sanitizeEndpoint('my app!'), 'my-app')
    assert.equal(sanitizeEndpoint('a---b'), 'a-b')
  })

  it('trims edge dashes, which would be invalid DNS labels', () => {
    assert.equal(sanitizeEndpoint('--dsh--'), 'dsh')
    assert.equal(sanitizeEndpoint('  spaced  '), 'spaced')
  })

  it('truncates to a DNS label without leaving a trailing dash', () => {
    const long = `${'a'.repeat(62)}-tail`
    const result = sanitizeEndpoint(long)
    assert.ok(result.length <= MAX_ENDPOINT_LENGTH)
    assert.equal(isValidEndpoint(result), true)
  })

  it('falls back when nothing usable survives', () => {
    assert.equal(sanitizeEndpoint('!!!'), 'dsh')
    assert.equal(sanitizeEndpoint('', { fallback: 'custom' }), 'custom')
    assert.equal(sanitizeEndpoint(undefined, { fallback: 'custom' }), 'custom')
  })
})

describe('randomEndpoint', () => {
  it('produces valid, distinct endpoints', () => {
    const seen = new Set()
    for (let i = 0; i < 200; i += 1) {
      const endpoint = randomEndpoint()
      assert.equal(isValidEndpoint(endpoint), true, `${endpoint} should be valid`)
      assert.match(endpoint, /^dsh-[a-z0-9]{6}$/)
      seen.add(endpoint)
    }
    // Collisions are not impossible, but 200 draws sharing fewer than 190
    // distinct values would mean the generator is broken.
    assert.ok(seen.size > 190, `only ${seen.size} distinct endpoints`)
  })

  it('honours a custom prefix and length', () => {
    assert.match(randomEndpoint('probe', 4), /^probe-[a-z0-9]{4}$/)
  })
})

describe('remoteUrlFor', () => {
  it('uses a subdomain in subdomain mode', () => {
    assert.equal(
      remoteUrlFor({ endpoint: 'dsh-a1b2c3', remote: 'https://clauded.friddle.me' }),
      'https://dsh-a1b2c3.clauded.friddle.me/',
    )
  })

  it('uses a path prefix in path mode', () => {
    assert.equal(
      remoteUrlFor({ endpoint: 'dsh-a1b2c3', remote: 'https://clauded.friddle.me', mode: 'path' }),
      'https://clauded.friddle.me/dsh-a1b2c3/',
    )
  })

  it('keeps a non-default port', () => {
    assert.equal(
      remoteUrlFor({ endpoint: 'ep', remote: 'http://127.0.0.1:8001' }),
      'http://ep.127.0.0.1:8001/',
    )
  })

  it('rejects a remote that is not an absolute URL', () => {
    assert.throws(() => remoteUrlFor({ endpoint: 'ep', remote: 'clauded.friddle.me' }), TypeError)
  })
})
