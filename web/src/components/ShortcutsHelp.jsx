import { useEffect } from 'react'
import { SHORTCUTS } from '../lib/shortcuts.js'
import { CloseIcon } from './Icons.jsx'

const keyCap = (k) => (k === '?' ? '?' : k.toUpperCase())

// A small, dismissible cheat-sheet of the in-call keyboard shortcuts. Opened
// with "?" or from the participant chrome. Push-to-talk is listed explicitly
// since it is a hold, not a tap.
export default function ShortcutsHelp({ onClose }) {
  useEffect(() => {
    const onKey = (e) => {
      if (e.key === 'Escape') onClose()
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [onClose])

  return (
    <div className="shortcuts-scrim" role="dialog" aria-modal="true" aria-label="Keyboard shortcuts" onClick={onClose}>
      <div className="shortcuts-card" onClick={(e) => e.stopPropagation()}>
        <header className="panel-head">
          <h2>Keyboard shortcuts</h2>
          <button type="button" className="icon-btn" aria-label="Close shortcuts" onClick={onClose}>
            <CloseIcon width={18} height={18} />
          </button>
        </header>
        <ul className="shortcut-list">
          {SHORTCUTS.map((s) => (
            <li key={s.action}>
              <span className="shortcut-label">{s.label}</span>
              <kbd>{keyCap(s.keys[0])}</kbd>
            </li>
          ))}
          <li>
            <span className="shortcut-label">Push to talk (hold)</span>
            <kbd>Space</kbd>
          </li>
        </ul>
      </div>
    </div>
  )
}
