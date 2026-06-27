import { describe, it, expect, vi } from 'vitest'
import {
  createChatTransport,
  hasTalkBinding,
  EphemeralChatTransport,
  TalkChatTransport,
} from './chatTransport.js'

// A fake LiveKit data-channel wire: captures published chat and lets a test
// inject inbound chat without a real Room.
function fakeWire() {
  const published = []
  let inbound = null
  return {
    published,
    publishChat: (text) => published.push(text),
    onChatData: (cb) => {
      inbound = cb
      return () => {
        inbound = null
      }
    },
    deliver: (from, text) => inbound?.(from, text),
  }
}

describe('hasTalkBinding', () => {
  it('requires both a channel id and a base URL', () => {
    expect(hasTalkBinding(null)).toBe(false)
    expect(hasTalkBinding({ channelId: 'c1' })).toBe(false)
    expect(hasTalkBinding({ base: 'https://talk' })).toBe(false)
    expect(hasTalkBinding({ channelId: 'c1', base: 'https://talk' })).toBe(true)
  })
})

describe('EphemeralChatTransport', () => {
  it('works with no Talk binding: echoes sent + ingests inbound, stays ephemeral', async () => {
    const wire = fakeWire()
    const t = await createChatTransport({ talk: null, wire })

    expect(t).toBeInstanceOf(EphemeralChatTransport)
    expect(t.synced).toBe(false)
    expect(t.label).toBe('ephemeral')

    const seen = []
    t.subscribe((msgs) => (seen.length = 0, seen.push(...msgs)))

    t.send('hi there')
    expect(wire.published).toEqual(['hi there'])
    expect(seen.at(-1)).toMatchObject({ text: 'hi there', self: true, from: 'You' })

    wire.deliver('Amara', 'hello back')
    expect(seen.at(-1)).toMatchObject({ text: 'hello back', self: false, from: 'Amara' })
  })
})

describe('createChatTransport — selection', () => {
  it('selects the Talk transport when bound and reachable', async () => {
    const fetchImpl = vi.fn(async () => ({
      ok: true,
      status: 200,
      json: async () => ({ messages: [{ id: '1', from: 'Sipho', text: 'history', ts: 1000 }] }),
    }))

    const t = await createChatTransport({
      talk: { channelId: 'c1', base: 'https://talk.example', token: 'tok' },
      wire: fakeWire(),
      fetchImpl,
    })

    expect(t).toBeInstanceOf(TalkChatTransport)
    expect(t.synced).toBe(true)
    expect(t.label).toBe('talk')
    // Pulled recent history on start, authenticated with the Bearer token.
    expect(t.history()).toEqual([{ id: '1', from: 'Sipho', text: 'history', ts: 1000, self: false }])
    const [url, opts] = fetchImpl.mock.calls[0]
    expect(url).toBe('https://talk.example/api/spaces/channels/c1/messages?limit=100')
    expect(opts.headers.Authorization).toBe('Bearer tok')
    t.dispose()
  })

  it('falls back to ephemeral when the Talk endpoint is unreachable', async () => {
    const fetchImpl = vi.fn(async () => {
      throw new Error('network down')
    })
    const t = await createChatTransport({
      talk: { channelId: 'c1', base: 'https://talk.example', token: 'tok' },
      wire: fakeWire(),
      fetchImpl,
    })
    expect(t).toBeInstanceOf(EphemeralChatTransport)
    expect(t.synced).toBe(false)
  })

  it('falls back to ephemeral when Talk rejects (e.g. 401)', async () => {
    const fetchImpl = vi.fn(async () => ({ ok: false, status: 401, json: async () => ({}) }))
    const t = await createChatTransport({
      talk: { channelId: 'c1', base: 'https://talk.example' },
      wire: fakeWire(),
      fetchImpl,
    })
    expect(t).toBeInstanceOf(EphemeralChatTransport)
  })
})

describe('TalkChatTransport', () => {
  it('posts a message then reconciles from the server, marking it self', async () => {
    let store = []
    const fetchImpl = vi.fn(async (url, opts) => {
      if (opts.method === 'POST') {
        const body = JSON.parse(opts.body)
        store = [...store, { id: '10', author: 'You', text: body.text, createdAt: 2000 }]
        return { ok: true, status: 200, json: async () => store.at(-1) }
      }
      return { ok: true, status: 200, json: async () => ({ messages: store }) }
    })

    const t = new TalkChatTransport({ channelId: 'c1', base: 'https://talk', token: '', fetchImpl })
    await t.start()
    await t.send('persisted line')

    expect(t.history().at(-1)).toMatchObject({ id: '10', text: 'persisted line', self: true })
    t.dispose()
  })
})
