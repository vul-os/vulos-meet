import { describe, it, expect } from 'vitest'
import { resolveShortcut, isPushToTalkKey, SHORTCUTS } from './shortcuts.js'

const ev = (over = {}) => ({ key: '', ctrlKey: false, metaKey: false, altKey: false, target: { tagName: 'BODY' }, ...over })

describe('resolveShortcut', () => {
  it('maps each registered key to its action', () => {
    for (const s of SHORTCUTS) {
      expect(resolveShortcut(ev({ key: s.keys[0] }))).toBe(s.action)
    }
  })

  it('is case-insensitive', () => {
    expect(resolveShortcut(ev({ key: 'M' }))).toBe('mic')
    expect(resolveShortcut(ev({ key: 'V' }))).toBe('cam')
  })

  it('ignores unknown keys', () => {
    expect(resolveShortcut(ev({ key: 'z' }))).toBeNull()
  })

  it('suppresses shortcuts while typing in a field', () => {
    expect(resolveShortcut(ev({ key: 'm', target: { tagName: 'INPUT' } }))).toBeNull()
    expect(resolveShortcut(ev({ key: 'm', target: { tagName: 'TEXTAREA' } }))).toBeNull()
    expect(resolveShortcut(ev({ key: 'm', target: { isContentEditable: true } }))).toBeNull()
  })

  it('never hijacks browser/OS chords (modifier held)', () => {
    expect(resolveShortcut(ev({ key: 'm', ctrlKey: true }))).toBeNull()
    expect(resolveShortcut(ev({ key: 'm', metaKey: true }))).toBeNull()
    expect(resolveShortcut(ev({ key: 'm', altKey: true }))).toBeNull()
  })
})

describe('isPushToTalkKey', () => {
  it('recognises Space by key or code', () => {
    expect(isPushToTalkKey(ev({ key: ' ' }))).toBe(true)
    expect(isPushToTalkKey(ev({ key: 'Spacebar', code: 'Space' }))).toBe(true)
  })

  it('is inert while typing', () => {
    expect(isPushToTalkKey(ev({ key: ' ', target: { tagName: 'INPUT' } }))).toBe(false)
  })

  it('is false for non-space keys', () => {
    expect(isPushToTalkKey(ev({ key: 'm' }))).toBe(false)
  })
})
