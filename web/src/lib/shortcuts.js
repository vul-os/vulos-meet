// Keyboard-shortcut resolution for the in-call UI, factored out of React so the
// mapping is unit-tested independently of the DOM.
//
// resolveShortcut(event-like) -> action name | null
// A synthetic "event-like" is { key, ctrlKey, metaKey, altKey, target } — we
// suppress shortcuts while the user is typing in a field or holding a modifier
// (so browser/OS chords are never hijacked).

export const SHORTCUTS = [
  { keys: ['m'], action: 'mic', label: 'Mute / unmute' },
  { keys: ['v'], action: 'cam', label: 'Camera on / off' },
  { keys: ['s'], action: 'screen', label: 'Share screen' },
  { keys: ['h'], action: 'hand', label: 'Raise / lower hand' },
  { keys: ['c'], action: 'chat', label: 'Toggle chat' },
  { keys: ['p'], action: 'people', label: 'Toggle participants' },
  { keys: ['e'], action: 'reactions', label: 'Reactions' },
  { keys: ['?'], action: 'help', label: 'Show shortcuts' },
]

const KEY_TO_ACTION = new Map()
for (const s of SHORTCUTS) for (const k of s.keys) KEY_TO_ACTION.set(k, s.action)

function isTypingTarget(target) {
  if (!target) return false
  const tag = (target.tagName || '').toLowerCase()
  return tag === 'input' || tag === 'textarea' || tag === 'select' || target.isContentEditable === true
}

export function resolveShortcut(e) {
  if (!e || e.ctrlKey || e.metaKey || e.altKey) return null
  if (isTypingTarget(e.target)) return null
  const key = (e.key || '').toLowerCase()
  return KEY_TO_ACTION.get(key) || null
}

// Push-to-talk lives on the spacebar: hold to talk while muted. We only treat
// Space as PTT when the user is not typing; the caller latches on keydown and
// releases on keyup.
export function isPushToTalkKey(e) {
  if (!e || isTypingTarget(e.target)) return false
  return e.key === ' ' || e.code === 'Space'
}
