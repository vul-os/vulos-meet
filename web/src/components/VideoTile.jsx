import { useEffect, useRef } from 'react'
import { MicOffIcon, HandIcon, SignalIcon, PinIcon, PinOffIcon } from './Icons.jsx'
import { qualityInfo, shouldShowQuality } from '../lib/quality.js'

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

export default function VideoTile({ tile, focus = false, pinned = false, onTogglePin }) {
  const ref = useRef(null)
  const track = tile.track
  const q = qualityInfo(tile.quality)

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

      {onTogglePin && (
        <button
          type="button"
          className={`tile-pin ${pinned ? 'on' : ''}`}
          aria-pressed={pinned}
          aria-label={pinned ? `Unpin ${tile.name}` : `Pin ${tile.name}`}
          title={pinned ? 'Unpin' : 'Pin'}
          onClick={onTogglePin}
        >
          {pinned ? <PinOffIcon width={15} height={15} /> : <PinIcon width={15} height={15} />}
        </button>
      )}

      <div className="tile-footer">
        <span className="tile-name">
          {tile.name}
          {tile.isLocal && tile.kind === 'camera' ? ' (you)' : ''}
        </span>
        <span className="tile-badges">
          {tile.kind === 'camera' && shouldShowQuality(tile.quality) && (
            <span className={`tile-signal ${q.tone}`} title={q.label} aria-label={q.label}>
              <SignalIcon bars={q.bars} width={15} height={15} />
            </span>
          )}
          {tile.kind === 'camera' && !tile.micOn && (
            <span className="tile-muted" title="Muted">
              <MicOffIcon width={15} height={15} />
            </span>
          )}
        </span>
      </div>
    </div>
  )
}
