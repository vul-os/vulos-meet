import { describe, it, expect } from 'vitest'
import { parseConfig, roomDisplayName, talkBinding } from './config.js'

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

  it('parses the Talk binding params (talkToken or talkSession alias)', () => {
    const c = parseConfig('?talkChannel=team:general&talkBase=https://talk.example&talkToken=jwt123', '/')
    expect(c.talkChannel).toBe('team:general')
    expect(c.talkBase).toBe('https://talk.example')
    expect(c.talkToken).toBe('jwt123')
    expect(parseConfig('?talkSession=sess9', '/').talkToken).toBe('sess9')
  })
})

describe('talkBinding', () => {
  it('returns null for a standalone meeting (no Talk params)', () => {
    expect(talkBinding(parseConfig('?room=acme:standup', '/'))).toBeNull()
    expect(talkBinding(parseConfig('?talkChannel=c1', '/'))).toBeNull()
  })

  it('builds a binding when channel + base are present', () => {
    const c = parseConfig('?talkChannel=c1&talkBase=https://talk&talkToken=t', '/')
    expect(talkBinding(c)).toEqual({ channelId: 'c1', base: 'https://talk', token: 't' })
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
