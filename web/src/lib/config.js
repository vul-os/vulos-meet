// Runtime configuration for the Vulos Meet client.
//
// The client is served from the public signal-gate listener (same origin as
// /rtc), so by default it connects LiveKit back to THIS origin. A token is
// obtained out-of-band (minted by vulos-cloud or any LiveKit-compatible minter
// — vulos-meet itself never mints) and handed to the client via the URL: this
// is exactly what Talk huddles and Mail/Calendar meeting links produce.
//
// Recognised URL inputs:
//   /<roomId>                       deep link — roomId from the path
//   ?room=<roomId>                  explicit room id
//   ?token=<jwt>                    a VULOS-MEET/1 access token
//   ?server=<wss url>              override the LiveKit signal URL
//   ?name=<display name>           prefill the display name
//   ?demo=<scene>                  offline demo mode (no SFU / no media)
//   ?talkChannel=<id>              bind in-call chat to a Talk channel (persistent)
//   ?talkBase=<url>                Talk API base URL for that channel
//   ?talkToken=<jwt>               Bearer credential for Talk's message API
//                                  (alias: ?talkSession=<token>)

/** Derive the LiveKit signal URL from the current origin (ws/wss + same host). */
export function defaultServerUrl() {
  if (typeof window === 'undefined') return ''
  const { protocol, host } = window.location
  const wsProto = protocol === 'https:' ? 'wss:' : 'ws:'
  return `${wsProto}//${host}`
}

/** A room id may carry a tenant prefix (<tenant><sep><rest>); show only the rest. */
export function roomDisplayName(roomId, separator = ':') {
  if (!roomId) return ''
  const i = roomId.indexOf(separator)
  return i >= 0 ? roomId.slice(i + 1) : roomId
}

export function parseConfig(search = window.location.search, pathname = window.location.pathname) {
  const params = new URLSearchParams(search)

  // Deep-link room id from the path: /<roomId> (ignore known asset roots).
  let pathRoom = ''
  const seg = decodeURIComponent(pathname.replace(/^\/+/, '').split('/')[0] || '')
  const reserved = new Set(['index.html', 'assets', 'favicon.svg', 'apps'])
  if (seg && !reserved.has(seg) && !seg.includes('.')) pathRoom = seg

  // The Apps & Bots management view: /apps or ?view=apps. It is a separate
  // surface from the call UI (a different concern: operator-managed apps & bots).
  const view = seg === 'apps' || params.get('view') === 'apps' ? 'apps' : 'call'

  const room = params.get('room') || pathRoom || ''
  return {
    view,
    room,
    token: params.get('token') || '',
    serverUrl: params.get('server') || defaultServerUrl(),
    displayName: params.get('name') || '',
    demo: params.get('demo') || '',
    separator: params.get('sep') || ':',
    talkChannel: params.get('talkChannel') || '',
    talkBase: params.get('talkBase') || '',
    talkToken: params.get('talkToken') || params.get('talkSession') || '',
  }
}

/**
 * Extract the Talk chat binding from a parsed config, or null when this is a
 * standalone meeting. A binding needs at least a channel id and a Talk base URL;
 * the token is optional (Talk may accept unauthenticated reads, or 401 and we
 * degrade to ephemeral chat).
 */
export function talkBinding(config) {
  if (!config || !config.talkChannel || !config.talkBase) return null
  return {
    channelId: config.talkChannel,
    base: config.talkBase,
    token: config.talkToken || '',
  }
}
