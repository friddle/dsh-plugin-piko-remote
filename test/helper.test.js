import assert from 'node:assert/strict'
import { describe, it } from 'node:test'

import { HelperMissingError, bundledHelperPath, helperFileName, resolveHelperPath } from '../lib/helper.js'

describe('helperFileName', () => {
  it('maps node platform and arch onto the Go build names', () => {
    assert.equal(helperFileName('linux', 'x64'), 'piko-expose-linux-amd64')
    assert.equal(helperFileName('linux', 'arm64'), 'piko-expose-linux-arm64')
    assert.equal(helperFileName('darwin', 'arm64'), 'piko-expose-darwin-arm64')
    assert.equal(helperFileName('win32', 'x64'), 'piko-expose-windows-amd64.exe')
  })

  it('refuses a platform with no build instead of guessing', () => {
    assert.throws(() => helperFileName('freebsd', 'x64'), /no piko-expose build/)
    assert.throws(() => helperFileName('linux', 'ia32'), /no piko-expose build/)
  })
})

describe('bundledHelperPath', () => {
  it('points inside this package bin/ directory', () => {
    const bundled = bundledHelperPath('linux', 'x64')
    assert.match(bundled, /\/bin\/piko-expose-linux-amd64$/)
  })
})

describe('resolveHelperPath', () => {
  it('returns the bundled binary when it is executable', () => {
    const bundled = bundledHelperPath('linux', 'x64')
    assert.equal(resolveHelperPath({ platform: 'linux', arch: 'x64', isExecutable: () => true }), bundled)
  })

  it('prefers an explicit override', () => {
    const resolved = resolveHelperPath({
      override: '/custom/piko-expose',
      platform: 'linux',
      arch: 'x64',
      isExecutable: (candidate) => candidate === '/custom/piko-expose',
    })
    assert.equal(resolved, '/custom/piko-expose')
  })

  it('names the checked path and the fix when nothing is executable', () => {
    try {
      resolveHelperPath({ platform: 'linux', arch: 'x64', isExecutable: () => false })
      assert.fail('expected resolveHelperPath to throw')
    } catch (error) {
      assert.ok(error instanceof HelperMissingError, `unexpected error: ${error}`)
      assert.match(error.message, /build-helper\.sh/)
      assert.match(error.attempted, /piko-expose-linux-amd64$/)
    }
  })
})
