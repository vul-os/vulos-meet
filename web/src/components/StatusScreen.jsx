import { Logo } from './Logo.jsx'
import { Spinner } from './Icons.jsx'

// Full-screen state surfaces: connecting, reconnecting, room ended, left,
// error, permission-denied. Kept calm and explanatory — every state tells the
// user what happened and what to do next.
const COPY = {
  connecting: {
    title: 'Connecting…',
    body: 'Joining the meeting. This only takes a moment.',
    spinner: true,
  },
  reconnecting: {
    title: 'Reconnecting…',
    body: 'The connection dropped. Trying to restore your meeting.',
    spinner: true,
  },
  ended: {
    title: 'Meeting ended',
    body: 'The host ended this meeting for everyone.',
  },
  left: {
    title: 'You left the meeting',
    body: 'You can rejoin from the same link any time it is still active.',
  },
  'permission-denied': {
    title: 'Camera & microphone blocked',
    body: 'Your browser denied access to media devices. Allow them in the site permissions, then rejoin.',
  },
  error: {
    title: "Couldn't join the meeting",
    body: 'Something went wrong while connecting.',
  },
}

export default function StatusScreen({ status, message, onRetry, onRejoin }) {
  const c = COPY[status] || COPY.error
  const canRetry = status === 'error' || status === 'permission-denied'
  const canRejoin = status === 'left' || status === 'ended'

  return (
    <div className="status-screen">
      <div className={`status-card ${c.spinner ? 'live' : ''}`}>
        <div className="status-logo">
          <Logo size={26} />
        </div>
        {c.spinner && (
          <div className="status-spinner" aria-hidden>
            <Spinner width={34} height={34} />
          </div>
        )}
        <h1 className="status-title">{c.title}</h1>
        <p className="status-body">{message || c.body}</p>
        <div className="status-actions">
          {canRetry && onRetry && (
            <button type="button" className="btn primary" onClick={onRetry}>
              Try again
            </button>
          )}
          {canRejoin && onRejoin && (
            <button type="button" className="btn primary" onClick={onRejoin}>
              Rejoin
            </button>
          )}
        </div>
      </div>
      <p className="status-foot">
        <span className="dot-live" aria-hidden /> Vulos Meet — self-hostable video on the LiveKit SFU
      </p>
    </div>
  )
}
