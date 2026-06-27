// Pluggable in-call chat transports.
//
// Meet's chat panel is transport-agnostic: it renders a `messages[]` list and
// calls `send(text)`. Two implementations satisfy the same contract:
//
//   • EphemeralChatTransport — rides the LiveKit data channel. Messages live for
//     the duration of the call only and vanish when everyone leaves. This is the
//     default and keeps standalone meetings working with zero Talk dependency.
//
//   • TalkChatTransport — reads/writes a Talk channel over Talk's message API, so
//     the in-call conversation is persistent and continues in Talk after the
//     call. Selected only when the join context carries a Talk binding AND the
//     Talk endpoint is reachable; otherwise we degrade to ephemeral.
//
// ChatTransport interface (duck-typed):
//   send(text)            -> post a message
//   subscribe(cb)         -> cb(messages[]) on every change; returns unsubscribe
//   history()             -> current messages[]
//   synced  (boolean)     -> true when messages persist (Talk-backed)
//   label   ('talk'|'ephemeral')
//   dispose()             -> release listeners / timers
//
// A message is { id, from, text, ts, self }.

function cryptoId() {
  return Math.random().toString(36).slice(2) + Date.now().toString(36)
}

class BaseChatTransport {
  constructor() {
    this.messages = []
    this._listeners = new Set()
  }

  subscribe(cb) {
    this._listeners.add(cb)
    cb(this.messages)
    return () => this._listeners.delete(cb)
  }

  history() {
    return this.messages
  }

  _emit() {
    for (const cb of this._listeners) cb(this.messages)
  }

  _set(messages) {
    this.messages = messages
    this._emit()
  }

  _append(msg) {
    this._set([...this.messages, msg])
  }
}

// ---- ephemeral (LiveKit data channel) ----
//
// `wire` is a thin seam onto the LiveKit room provided by LiveRoom:
//   publishChat(text)  -> publish a chat JSON envelope on the 'chat' data topic
//   onChatData(cb)     -> cb(from, text) for inbound chat envelopes; returns off
// Keeping the wire abstract means this transport never imports livekit-client and
// is trivially unit-testable with a fake wire.
export class EphemeralChatTransport extends BaseChatTransport {
  constructor(wire) {
    super()
    this.synced = false
    this.label = 'ephemeral'
    this._wire = wire
    this._off = wire?.onChatData?.((from, text) => {
      this._append({ id: cryptoId(), from: from || 'Guest', text: String(text || ''), ts: Date.now(), self: false })
    })
  }

  send(text) {
    const t = String(text || '').trim()
    if (!t) return
    this._wire?.publishChat?.(t)
    this._append({ id: cryptoId(), from: 'You', text: t, ts: Date.now(), self: true })
  }

  dispose() {
    this._off?.()
    this._listeners.clear()
  }
}

// ---- Talk-backed (persistent) ----
//
// Talk binding: { channelId, base, token, self }. We talk to Talk's REST message
// API under `${base}/api/spaces/channels/{channelId}/messages`:
//   GET  …?limit=N   -> { messages: [...] } | [...]   (recent history)
//   POST …           -> the created message            (send)
// Auth is the provided token as a Bearer credential. Subscription is poll-based
// (Talk's data plane is its own concern; the client just refreshes) so it works
// against any Talk deployment without a socket dependency.
export class TalkChatTransport extends BaseChatTransport {
  constructor({ channelId, base, token, self, fetchImpl, pollMs = 4000 } = {}) {
    super()
    this.synced = true
    this.label = 'talk'
    this.channelId = channelId
    this.base = String(base || '').replace(/\/+$/, '')
    this.token = token || ''
    this.self = self || ''
    this._fetch = fetchImpl || (typeof fetch !== 'undefined' ? fetch.bind(globalThis) : null)
    this._pollMs = pollMs
    this._timer = null
    this._selfIds = new Set()
  }

  // Fetch recent history once; throws if the endpoint is unreachable / rejects so
  // the selector can fall back to ephemeral. On success, start polling.
  async start() {
    await this._refresh()
    this._timer = setInterval(() => {
      this._refresh().catch(() => {})
    }, this._pollMs)
    return this
  }

  async send(text) {
    const t = String(text || '').trim()
    if (!t) return
    // Optimistic echo so the sender sees their line immediately.
    const tmp = { id: 'tmp-' + cryptoId(), from: 'You', text: t, ts: Date.now(), self: true }
    this._append(tmp)
    try {
      const created = await this._req('POST', '', { text: t })
      if (created && created.id != null) this._selfIds.add(String(created.id))
      await this._refresh()
    } catch {
      // Network blip: keep the optimistic line; the next poll reconciles.
    }
  }

  async _refresh() {
    const res = await this._req('GET', '?limit=100')
    const list = Array.isArray(res) ? res : res?.messages || res?.items || []
    this._set(list.map((m) => this._normalize(m)).sort((a, b) => a.ts - b.ts))
  }

  _normalize(m) {
    const id = String(m.id ?? cryptoId())
    const from = m.author?.name || m.author || m.from || m.user || m.sender || 'Guest'
    const raw = m.text ?? m.body ?? m.content ?? ''
    let ts = m.ts ?? m.createdAt ?? m.created_at ?? m.time
    ts = typeof ts === 'number' ? ts : ts ? Date.parse(ts) || Date.now() : Date.now()
    const self = this._selfIds.has(id) || (!!this.self && from === this.self)
    return { id, from: String(from), text: String(raw), ts, self }
  }

  async _req(method, suffix, body) {
    if (!this._fetch) throw new Error('no fetch available')
    const url = `${this.base}/api/spaces/channels/${encodeURIComponent(this.channelId)}/messages${suffix}`
    const headers = { 'Content-Type': 'application/json' }
    if (this.token) headers.Authorization = `Bearer ${this.token}`
    const res = await this._fetch(url, {
      method,
      headers,
      body: body ? JSON.stringify(body) : undefined,
    })
    if (!res.ok) throw new Error(`talk ${method} ${res.status}`)
    if (res.status === 204) return null
    return res.json()
  }

  dispose() {
    if (this._timer) clearInterval(this._timer)
    this._timer = null
    this._listeners.clear()
  }
}

// ---- selection ----
//
// Talk binding present + reachable -> TalkChatTransport (persistent, synced to
// Talk). Otherwise -> EphemeralChatTransport (data channel). This is the single
// decision point used by LiveRoom and pinned by tests.
export function hasTalkBinding(talk) {
  return !!(talk && talk.channelId && talk.base)
}

export async function createChatTransport({ talk, wire, fetchImpl } = {}) {
  if (hasTalkBinding(talk)) {
    const t = new TalkChatTransport({
      channelId: talk.channelId,
      base: talk.base,
      token: talk.token,
      self: talk.self,
      fetchImpl,
    })
    try {
      await t.start()
      return t
    } catch {
      // Talk endpoint unreachable — degrade gracefully to ephemeral.
      t.dispose()
    }
  }
  return new EphemeralChatTransport(wire)
}
