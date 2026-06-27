import { describe, it, expect } from 'vitest'
import { LiveRoom } from './liveRoom.js'

// The whiteboard rides the same LiveKit data channel as chat. These tests pin
// the topic routing contract: raw Yjs/awareness bytes must reach board listeners
// and never hit chat's JSON parser, and chat JSON must never reach the board.
describe('LiveRoom data-channel topic routing', () => {
  it('dispatches every board topic to board listeners and not to chat', () => {
    const room = new LiveRoom()
    const seen = []
    room.onBoardData((topic, bytes) => seen.push({ topic, bytes }))

    const payload = new Uint8Array([1, 2, 3])
    for (const topic of ['board', 'board-aware', 'board-ctl']) {
      room._onData(payload, { name: 'Peer', identity: 'p1' }, topic)
    }

    expect(seen.map((s) => s.topic)).toEqual(['board', 'board-aware', 'board-ctl'])
    expect(seen.every((s) => s.bytes === payload)).toBe(true)
    // None of the raw board bytes leaked into chat.
    expect(room.messages).toHaveLength(0)
  })

  it('routes chat JSON to messages and never to board listeners', () => {
    const room = new LiveRoom()
    let boardHits = 0
    room.onBoardData(() => boardHits++)

    const chat = new TextEncoder().encode(JSON.stringify({ kind: 'chat', text: 'hi' }))
    room._onData(chat, { name: 'Amara', identity: 'a1' }, undefined)

    expect(boardHits).toBe(0)
    expect(room.messages).toHaveLength(1)
    expect(room.messages[0]).toMatchObject({ from: 'Amara', text: 'hi', self: false })
  })

  it('ignores malformed non-board data without throwing', () => {
    const room = new LiveRoom()
    expect(() => room._onData(new Uint8Array([0xff, 0x00]), {}, undefined)).not.toThrow()
    expect(room.messages).toHaveLength(0)
  })
})
