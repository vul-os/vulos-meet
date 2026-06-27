import { describe, it, expect } from 'vitest'
import { DemoRoom } from './demoRoom.js'

describe('DemoRoom', () => {
  it('seeds an in-room scene with tiles and participants', () => {
    const r = new DemoRoom('in-room')
    let snap
    r.on((s) => (snap = s))
    expect(snap.status).toBe('connected')
    expect(snap.participants.length).toBeGreaterThan(1)
    expect(snap.tiles.length).toBeGreaterThan(1)
    expect(snap.tiles.some((t) => t.isLocal)).toBe(true)
  })

  it('produces a presenter tile in the screen-share scene', () => {
    const r = new DemoRoom('screen-share')
    let snap
    r.on((s) => (snap = s))
    expect(snap.presenter).toBeTruthy()
    expect(snap.tiles.some((t) => t.kind === 'screen')).toBe(true)
  })

  it('maps non-connected scenes to status surfaces', () => {
    for (const scene of ['connecting', 'reconnecting', 'ended', 'error', 'permission-denied']) {
      const r = new DemoRoom(scene)
      let snap
      r.on((s) => (snap = s))
      expect(snap.status).toBe(scene)
    }
  })

  it('toggles local mic and reflects it in the local tile', () => {
    const r = new DemoRoom('in-room')
    let snap
    r.on((s) => (snap = s))
    const before = snap.local.micOn
    r.toggleMic()
    expect(snap.local.micOn).toBe(!before)
    const localTile = snap.tiles.find((t) => t.isLocal && t.kind === 'camera')
    expect(localTile.micOn).toBe(!before)
  })

  it('appends sent chat messages', () => {
    const r = new DemoRoom('in-room')
    let snap
    r.on((s) => (snap = s))
    const n = snap.messages.length
    r.sendChat('hello team')
    expect(snap.messages.length).toBe(n + 1)
    expect(snap.messages.at(-1)).toMatchObject({ self: true, text: 'hello team' })
  })
})
