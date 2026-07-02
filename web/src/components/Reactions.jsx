import { useEffect, useRef, useState } from 'react'
import { REACTIONS } from '../lib/liveRoom.js'
import { SmileIcon } from './Icons.jsx'

let seq = 0

// ReactionsOverlay renders emoji that float up and fade — a lightweight,
// non-blocking acknowledgement layer over the stage. It subscribes to the room
// controller's reaction stream via `actions.onReaction`.
export function ReactionsOverlay({ actions }) {
  const [live, setLive] = useState([])

  useEffect(() => {
    const off = actions.onReaction?.((r) => {
      const id = `r${seq++}`
      // Spread the launch point so a burst doesn't stack into one column.
      const left = 12 + Math.random() * 70
      setLive((cur) => [...cur, { id, emoji: r.emoji, from: r.from, left }])
      setTimeout(() => setLive((cur) => cur.filter((x) => x.id !== id)), 2600)
    })
    return off
  }, [actions])

  if (!live.length) return null
  return (
    <div className="reactions-overlay" aria-live="polite">
      {live.map((r) => (
        <span key={r.id} className="reaction-fly" style={{ left: `${r.left}%` }} title={`${r.from} reacted`}>
          {r.emoji}
        </span>
      ))}
    </div>
  )
}

// ReactionMenu is the emoji picker opened from the control bar. `open` is
// controlled by the parent so a keyboard shortcut can toggle it too.
export function ReactionMenu({ open, onClose, onPick }) {
  const ref = useRef(null)
  useEffect(() => {
    if (!open) return
    const onDoc = (e) => {
      if (ref.current && !ref.current.contains(e.target)) onClose()
    }
    const onKey = (e) => e.key === 'Escape' && onClose()
    document.addEventListener('mousedown', onDoc)
    document.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('mousedown', onDoc)
      document.removeEventListener('keydown', onKey)
    }
  }, [open, onClose])

  if (!open) return null
  return (
    <div className="reaction-menu" ref={ref} role="menu" aria-label="Reactions">
      {REACTIONS.map((emoji) => (
        <button
          key={emoji}
          type="button"
          className="reaction-btn"
          role="menuitem"
          aria-label={`React ${emoji}`}
          onClick={() => {
            onPick(emoji)
            onClose()
          }}
        >
          {emoji}
        </button>
      ))}
    </div>
  )
}

export { SmileIcon }
