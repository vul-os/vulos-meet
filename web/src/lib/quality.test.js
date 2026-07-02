import { describe, it, expect } from 'vitest'
import { qualityInfo, shouldShowQuality } from './quality.js'

describe('qualityInfo', () => {
  it('maps each known label to bars + tone', () => {
    expect(qualityInfo('excellent')).toMatchObject({ bars: 4, tone: 'ok' })
    expect(qualityInfo('good')).toMatchObject({ bars: 3, tone: 'ok' })
    expect(qualityInfo('poor')).toMatchObject({ bars: 1, tone: 'warn' })
    expect(qualityInfo('lost')).toMatchObject({ bars: 0, tone: 'danger' })
  })

  it('falls back to unknown for unrecognised values', () => {
    expect(qualityInfo('bogus')).toEqual(qualityInfo('unknown'))
    expect(qualityInfo(undefined).tone).toBe('muted')
  })
})

describe('shouldShowQuality', () => {
  it('only surfaces actionable (degraded) states', () => {
    expect(shouldShowQuality('poor')).toBe(true)
    expect(shouldShowQuality('lost')).toBe(true)
    expect(shouldShowQuality('good')).toBe(false)
    expect(shouldShowQuality('excellent')).toBe(false)
    expect(shouldShowQuality('unknown')).toBe(false)
  })
})
