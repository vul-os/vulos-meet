import { describe, it, expect } from 'vitest'
import { parseConfig, roomDisplayName } from './config.js'

describe('parseConfig', () => {
  it('reads a room from the deep-link path', () => {
    const c = parseConfig('', '/acme:standup')
    expect(c.room).toBe('acme:standup')
  })

  it('prefers ?room= over the path and reads token/name', () => {
    const c = parseConfig('?room=acme:retro&token=abc.def.ghi&name=Amara', '/ignored')
    expect(c.room).toBe('acme:retro')
    expect(c.token).toBe('abc.def.ghi')
    expect(c.displayName).toBe('Amara')
  })

  it('ignores asset-like path segments', () => {
    const c = parseConfig('', '/index.html')
    expect(c.room).toBe('')
    const c2 = parseConfig('', '/assets/app.js')
    expect(c2.room).toBe('')
  })

  it('captures the demo scene', () => {
    expect(parseConfig('?demo=screen-share', '/').demo).toBe('screen-share')
  })
})

describe('roomDisplayName', () => {
  it('strips the tenant prefix', () => {
    expect(roomDisplayName('acme:standup-2026', ':')).toBe('standup-2026')
  })
  it('returns the id unchanged when no separator', () => {
    expect(roomDisplayName('standup', ':')).toBe('standup')
  })
  it('handles empty input', () => {
    expect(roomDisplayName('', ':')).toBe('')
  })
})
