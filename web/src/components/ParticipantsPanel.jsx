import { MicIcon, MicOffIcon, CamOffIcon, HandIcon, CloseIcon } from './Icons.jsx'

function initials(name) {
  return (name || '?')
    .split(/\s+/)
    .slice(0, 2)
    .map((w) => w[0])
    .join('')
    .toUpperCase()
}

export default function ParticipantsPanel({ participants, onClose }) {
  const sorted = [...participants].sort((a, b) => {
    if (a.handRaised !== b.handRaised) return a.handRaised ? -1 : 1
    if (a.isLocal !== b.isLocal) return a.isLocal ? -1 : 1
    return a.name.localeCompare(b.name)
  })
  return (
    <aside className="panel" aria-label="Participants">
      <header className="panel-head">
        <h2>
          Participants <span className="panel-count">{participants.length}</span>
        </h2>
        <button type="button" className="icon-btn" aria-label="Close participants" onClick={onClose}>
          <CloseIcon width={18} height={18} />
        </button>
      </header>
      <ul className="plist">
        {sorted.map((p) => (
          <li key={p.id} className="prow">
            <span className={`pavatar ${p.speaking ? 'speaking' : ''}`}>{initials(p.name)}</span>
            <span className="pname">
              {p.name}
              {p.isLocal ? ' (you)' : ''}
            </span>
            <span className="picons">
              {p.handRaised && (
                <span className="picon hand" title="Hand raised">
                  <HandIcon width={16} height={16} />
                </span>
              )}
              {!p.camOn && (
                <span className="picon" title="Camera off">
                  <CamOffIcon width={16} height={16} />
                </span>
              )}
              <span className={`picon ${p.micOn ? '' : 'off'}`} title={p.micOn ? 'Unmuted' : 'Muted'}>
                {p.micOn ? <MicIcon width={16} height={16} /> : <MicOffIcon width={16} height={16} />}
              </span>
            </span>
          </li>
        ))}
      </ul>
    </aside>
  )
}
