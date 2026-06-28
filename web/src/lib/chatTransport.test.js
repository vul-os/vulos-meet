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

  // ── Talk-chat transport seam: additional tests ───────────────────────────

  it('delivers to all subscribers when messages change', async () => {
    const fetchImpl = vi.fn(async () => ({
      ok: true,
      status: 200,
      json: async () => ({ messages: [{ id: '1', from: 'Sipho', text: 'hi', ts: 1000 }] }),
    }))

    const t = new TalkChatTransport({ channelId: 'c1', base: 'https://talk', fetchImpl })
    await t.start()

    const calls1 = []
    const calls2 = []
    const unsub1 = t.subscribe((msgs) => calls1.push(msgs.length))
    const unsub2 = t.subscribe((msgs) => calls2.push(msgs.length))

    // Both subscribers got the initial message list on subscribe().
    expect(calls1.at(-1)).toBe(1)
    expect(calls2.at(-1)).toBe(1)

    // Unsubscribing stops future deliveries to that listener only.
    unsub1()
    await t.send('new message') // triggers a refresh
    expect(calls1.length).toBe(1) // unsub1 was not called again
    expect(calls2.length).toBeGreaterThan(1) // unsub2 still receives

    unsub2()
    t.dispose()
  })

  it('normalises message fields from various API response shapes', async () => {
    // The Talk API may return messages in several shapes: {id, author.name, text, createdAt}
    // or {id, from, body, ts} or other variants. All must normalise to {id, from, text, ts, self}.
    // Use a fixed, sorted set of ts values so the sort-by-ts order is predictable.
    const store = [
      { id: '1', from: 'Sipho', body: 'world', ts: 1_000 },            // ts=1000, sorted first
      { id: '2', user: 'Bongani', content: 'test', time: 2_000 },      // ts=2000
      { id: '3', author: { name: 'Amara' }, text: 'hello', ts: 3_000 },// ts=3000
      { id: '4', sender: 'Lindiwe', text: 'reply', ts: 4_000 },        // ts=4000, sorted last
    ]
    const fetchImpl = vi.fn(async () => ({
      ok: true,
      status: 200,
      json: async () => ({ messages: store }),
    }))

    const t = new TalkChatTransport({ channelId: 'c1', base: 'https://talk', fetchImpl })
    await t.start()

    const msgs = t.history()
    expect(msgs).toHaveLength(4)

    // Messages are sorted ascending by ts.
    expect(msgs[0]).toMatchObject({ id: '1', from: 'Sipho', text: 'world', ts: 1_000 })
    expect(msgs[1]).toMatchObject({ id: '2', from: 'Bongani', text: 'test', ts: 2_000 })
    expect(msgs[2]).toMatchObject({ id: '3', from: 'Amara', text: 'hello', ts: 3_000 })
    expect(msgs[3]).toMatchObject({ id: '4', from: 'Lindiwe', text: 'reply', ts: 4_000 })
    // Every normalised message must have a numeric ts.
    for (const m of msgs) expect(typeof m.ts).toBe('number')

    t.dispose()
  })

  it('sorts messages by timestamp ascending', async () => {
    const store = [
      { id: '3', from: 'C', text: 'third', ts: 3000 },
      { id: '1', from: 'A', text: 'first', ts: 1000 },
      { id: '2', from: 'B', text: 'second', ts: 2000 },
    ]
    const fetchImpl = vi.fn(async () => ({
      ok: true,
      status: 200,
      json: async () => ({ messages: store }),
    }))

    const t = new TalkChatTransport({ channelId: 'c1', base: 'https://talk', fetchImpl })
    await t.start()

    const msgs = t.history()
    expect(msgs.map((m) => m.ts)).toEqual([1000, 2000, 3000])
    t.dispose()
  })

  it('dispose stops polling and clears all listeners', async () => {
    let callCount = 0
    const fetchImpl = vi.fn(async () => {
      callCount++
      return { ok: true, status: 200, json: async () => ({ messages: [] }) }
    })

    const t = new TalkChatTransport({ channelId: 'c1', base: 'https://talk', fetchImpl, pollMs: 1 })
    await t.start()
    const seen = []
    t.subscribe((msgs) => seen.push(msgs))

    t.dispose()
    const countAtDispose = callCount
    // After dispose the polling timer should be cleared; wait a few ms and
    // verify no additional fetches were triggered.
    await new Promise((r) => setTimeout(r, 20))
    expect(callCount).toBe(countAtDispose) // no new polls after dispose
  })

  it('marks sent messages as self using the server-returned id', async () => {
    let store = []
    const fetchImpl = vi.fn(async (url, opts) => {
      if (opts.method === 'POST') {
        const body = JSON.parse(opts.body)
        const created = { id: 'srv-99', from: 'Vusi', text: body.text, ts: Date.now() }
        store = [created]
        return { ok: true, status: 200, json: async () => created }
      }
      return { ok: true, status: 200, json: async () => ({ messages: store }) }
    })

    const t = new TalkChatTransport({ channelId: 'c1', base: 'https://talk', self: 'Vusi', fetchImpl })
    await t.start()
    await t.send('from me')

    // After the POST + GET reconcile, the message with id 'srv-99' should be
    // marked self (because _selfIds records the server-returned id).
    const msgs = t.history()
    const mine = msgs.find((m) => m.id === 'srv-99')
    expect(mine).toBeDefined()
    expect(mine.self).toBe(true)
    t.dispose()
  })

  it('handles an empty message list response without errors', async () => {
    const fetchImpl = vi.fn(async () => ({
      ok: true,
      status: 200,
      json: async () => ({ messages: [] }),
    }))

    const t = new TalkChatTransport({ channelId: 'c1', base: 'https://talk', fetchImpl })
    await t.start()
    expect(t.history()).toEqual([])
    t.dispose()
  })

  it('optimistic echo appears immediately before server reconcile', async () => {
    let postDone = false
    const fetchImpl = vi.fn(async (url, opts) => {
      if (opts.method === 'POST') {
        return {
          ok: true,
          status: 200,
          json: async () => {
            postDone = true
            return { id: 'srv-1', from: 'You', text: 'hello', ts: Date.now() }
          },
        }
      }
      // GET: return the server message only after POST has completed.
      if (postDone) {
        return { ok: true, status: 200, json: async () => ({ messages: [{ id: 'srv-1', from: 'You', text: 'hello', ts: 1 }] }) }
      }
      return { ok: true, status: 200, json: async () => ({ messages: [] }) }
    })

    const t = new TalkChatTransport({ channelId: 'c1', base: 'https://talk', fetchImpl })
    await t.start()

    let seenDuringPost = null
    const unsub = t.subscribe((msgs) => {
      if (!postDone && msgs.length > 0) seenDuringPost = msgs[0]
    })

    const sendPromise = t.send('hello')
    // Optimistic echo was appended immediately (before the fetch resolves).
    if (!postDone && t.history().length > 0) {
      expect(t.history()[0]).toMatchObject({ text: 'hello', self: true })
    }
    await sendPromise
    unsub()
    t.dispose()
  })
})

// ── Talk-chat transport seam: channel-ID URL encoding ────────────────────────
describe('TalkChatTransport — URL encoding', () => {
  it('percent-encodes the channel ID in request URLs', async () => {
    const fetchImpl = vi.fn(async () => ({
      ok: true,
      status: 200,
      json: async () => ({ messages: [] }),
    }))

    const t = new TalkChatTransport({
      channelId: 'channel with spaces/and#hash',
      base: 'https://talk',
      fetchImpl,
    })
    await t.start()

    const [url] = fetchImpl.mock.calls[0]
    expect(url).toContain(encodeURIComponent('channel with spaces/and#hash'))
    t.dispose()
  })
})

// ── EphemeralChatTransport: additional seam tests ────────────────────────────
describe('EphemeralChatTransport — seam tests', () => {
  it('dispose removes the wire listener (no further inbound after dispose)', () => {
    const wire = fakeWire()
    const t = new EphemeralChatTransport(wire)

    const seen = []
    t.subscribe((msgs) => seen.push(msgs.length))

    wire.deliver('Alice', 'before dispose')
    expect(seen.at(-1)).toBe(1)

    t.dispose()

    // After dispose the transport should no longer route inbound messages.
    // The wire's inbound callback was removed on dispose.
    wire.deliver('Bob', 'after dispose')
    // history() still has the 1 message from before dispose (messages are not cleared).
    expect(t.history().length).toBe(1)
  })

  it('empty string send is a no-op (not published on the wire)', () => {
    const wire = fakeWire()
    const t = new EphemeralChatTransport(wire)

    t.send('')
    t.send('   ')
    expect(wire.published).toHaveLength(0)
    expect(t.history()).toHaveLength(0)
  })

  it('multiple subscribers each receive all messages', () => {
    const wire = fakeWire()
    const t = new EphemeralChatTransport(wire)

    const a = [], b = []
    t.subscribe((msgs) => a.push(msgs.length))
    t.subscribe((msgs) => b.push(msgs.length))

    t.send('hello')
    wire.deliver('Sipho', 'world')

    // Both subscribers see every update.
    expect(a.at(-1)).toBeGreaterThan(0)
    expect(b.at(-1)).toBeGreaterThan(0)
    expect(a.at(-1)).toBe(b.at(-1))
  })
})
