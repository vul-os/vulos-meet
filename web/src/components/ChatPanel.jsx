import { useEffect, useRef, useState } from 'react'
import { CloseIcon, SendIcon } from './Icons.jsx'

function fmtTime(ts) {
  try {
    return new Date(ts).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
  } catch {
    return ''
  }
}

export default function ChatPanel({ messages, onSend, onClose }) {
  const [text, setText] = useState('')
  const endRef = useRef(null)

  useEffect(() => {
    endRef.current?.scrollIntoView({ block: 'end' })
  }, [messages])

  const submit = (e) => {
    e.preventDefault()
    const t = text.trim()
    if (!t) return
    onSend(t)
    setText('')
  }

  return (
    <aside className="panel" aria-label="In-call chat">
      <header className="panel-head">
        <h2>Chat</h2>
        <button type="button" className="icon-btn" aria-label="Close chat" onClick={onClose}>
          <CloseIcon width={18} height={18} />
        </button>
      </header>
      <div className="chat-log" role="log" aria-live="polite">
        {messages.length === 0 && <p className="chat-empty">No messages yet. Say hello.</p>}
        {messages.map((m) => (
          <div key={m.id} className={`chat-msg ${m.self ? 'self' : ''}`}>
            <div className="chat-meta">
              <span className="chat-from">{m.self ? 'You' : m.from}</span>
              <span className="chat-time">{fmtTime(m.ts)}</span>
            </div>
            <div className="chat-bubble">{m.text}</div>
          </div>
        ))}
        <div ref={endRef} />
      </div>
      <form className="chat-input" onSubmit={submit}>
        <input
          type="text"
          value={text}
          onChange={(e) => setText(e.target.value)}
          placeholder="Send a message"
          aria-label="Message"
          maxLength={2000}
        />
        <button type="submit" className="icon-btn send" aria-label="Send message" disabled={!text.trim()}>
          <SendIcon width={18} height={18} />
        </button>
      </form>
    </aside>
  )
}
