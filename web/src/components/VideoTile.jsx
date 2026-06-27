import { useEffect, useRef } from 'react'
import { MicOffIcon, HandIcon } from './Icons.jsx'

function initials(name) {
  return (name || '?')
    .split(/\s+/)
    .slice(0, 2)
    .map((w) => w[0])
    .join('')
    .toUpperCase()
}

// A demo "video" surface: a soft hue gradient standing in for a live camera so
// screenshots render meaningfully with no media devices. A screen-share demo
// gets a mock window-chrome dashboard instead of a face placeholder.
function DemoSurface({ track }) {
  if (track.kind === 'screen') {
    return (
      <div className="demo-screen" aria-hidden>
        <div className="demo-screen-bar">
          <span className="dot" />
          <span className="dot" />
          <span className="dot" />
          <span className="demo-screen-title">{track.label}</span>
        </div>
        <div className="demo-screen-body">
          <div className="demo-chart">
            {[42, 64, 38, 80, 56, 72, 48, 90, 60].map((h, i) => (
              <span key={i} style={{ height: `${h}%` }} />
            ))}
          </div>
          <div className="demo-lines">
            <span style={{ width: '70%' }} />
            <span style={{ width: '52%' }} />
            <span style={{ width: '61%' }} />
          </div>
        </div>
      </div>
    )
  }
  return (
    <div
      className="demo-cam"
      aria-hidden
      style={{
        background: `radial-gradient(120% 120% at 30% 20%, hsl(${track.hue} 45% 28%), hsl(${track.hue + 20} 35% 12%))`,
      }}
    />
  )
}

export default function VideoTile({ tile, focus = false }) {
  const ref = useRef(null)
  const track = tile.track

  useEffect(() => {
    const el = ref.current
    // Real LiveKit tracks expose attach/detach; demo descriptors do not.
    if (el && track && typeof track.attach === 'function') {
      track.attach(el)
      el.muted = true
      return () => {
        try {
          track.detach(el)
        } catch {
          /* noop */
        }
      }
    }
  }, [track])

  const isDemo = track && track.demo === true
  const isReal = track && typeof track.attach === 'function'
  const showVideo = !!track

  return (
    <div
      className={`tile ${tile.speaking ? 'speaking' : ''} ${focus ? 'focus' : ''} ${tile.kind === 'screen' ? 'screen' : ''}`}
      data-tile={tile.key}
    >
      {showVideo ? (
        isReal ? (
          <video ref={ref} autoPlay playsInline className="tile-video" />
        ) : isDemo ? (
          <DemoSurface track={track} />
        ) : null
      ) : (
        <div className="tile-avatar" aria-hidden>
          <span>{initials(tile.name)}</span>
        </div>
      )}

      {tile.handRaised && (
        <div className="tile-hand" title="Hand raised">
          <HandIcon width={16} height={16} />
        </div>
      )}

      <div className="tile-footer">
        <span className="tile-name">
          {tile.name}
          {tile.isLocal && tile.kind === 'camera' ? ' (you)' : ''}
        </span>
        {tile.kind === 'camera' && !tile.micOn && (
          <span className="tile-muted" title="Muted">
            <MicOffIcon width={15} height={15} />
          </span>
        )}
      </div>
    </div>
  )
}
