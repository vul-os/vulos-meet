import { useEffect, useRef, useState } from 'react'
import { Logo } from '../components/Logo.jsx'
import { MicIcon, MicOffIcon, CamIcon, CamOffIcon, LinkIcon, CheckIcon, ChevronDownIcon } from '../components/Icons.jsx'
import { enumerate, getPreviewStream, stopStream } from '../lib/devices.js'

function DeviceSelect({ icon, value, options, onChange, label }) {
  return (
    <div className="prejoin-device">
      <span className="prejoin-device-icon">{icon}</span>
      <div className="select flush">
        <select value={value || ''} onChange={(e) => onChange(e.target.value)} aria-label={label}>
          {(!options || options.length === 0) && <option value="">System default</option>}
          {options?.map((o) => (
            <option key={o.deviceId} value={o.deviceId}>
              {o.label}
            </option>
          ))}
        </select>
        <ChevronDownIcon width={14} height={14} className="select-caret" />
      </div>
    </div>
  )
}

export default function PreJoin({ config, onJoin }) {
  const videoRef = useRef(null)
  const streamRef = useRef(null)

  const [name, setName] = useState(config.displayName || '')
  const [token, setToken] = useState(config.token || '')
  const [room, setRoom] = useState(config.room || '')
  const [camOn, setCamOn] = useState(true)
  const [micOn, setMicOn] = useState(true)
  const [devices, setDevices] = useState({ cameras: [], mics: [], speakers: [] })
  const [camId, setCamId] = useState('')
  const [micId, setMicId] = useState('')
  const [permError, setPermError] = useState('')
  const [copied, setCopied] = useState(false)

  // (Re)acquire the preview stream when the camera/mic toggles or device picks
  // change. The lobby preview is a plain getUserMedia stream — no LiveKit Room
  // exists yet, so nothing leaves the device.
  useEffect(() => {
    let cancelled = false
    async function run() {
      stopStream(streamRef.current)
      streamRef.current = null
      if (videoRef.current) videoRef.current.srcObject = null
      if (!camOn && !micOn) return
      try {
        const stream = await getPreviewStream({ cameraId: camId, micId, video: camOn, audio: micOn })
        if (cancelled) {
          stopStream(stream)
          return
        }
        streamRef.current = stream
        setPermError('')
        if (videoRef.current && camOn) videoRef.current.srcObject = stream
        // Labels only populate after a grant — refresh the device list.
        const d = await enumerate()
        if (!cancelled) setDevices(d)
      } catch (err) {
        if (cancelled) return
        if (err.code === 'permission-denied') setPermError('Camera/microphone access was blocked. You can still join — others just won’t see or hear you.')
        else if (err.code === 'not-found') setPermError('No camera or microphone found.')
        else setPermError('Could not start your camera or microphone.')
      }
    }
    run()
    return () => {
      cancelled = true
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [camOn, micOn, camId, micId])

  useEffect(() => () => stopStream(streamRef.current), [])

  const join = () => {
    stopStream(streamRef.current)
    streamRef.current = null
    onJoin({
      displayName: name.trim() || 'Guest',
      token: token.trim(),
      room: room.trim(),
      cameraId: camId,
      micId,
      cam: camOn,
      mic: micOn,
    })
  }

  const copyLink = async () => {
    try {
      await navigator.clipboard.writeText(window.location.href)
      setCopied(true)
      setTimeout(() => setCopied(false), 1600)
    } catch {
      /* clipboard blocked */
    }
  }

  const hasTokenInUrl = !!config.token
  const canJoin = (config.demo ? true : !!token.trim()) && (!!room.trim() || !!config.demo)

  return (
    <div className="prejoin">
      <header className="prejoin-top">
        <Logo size={26} />
        <button type="button" className="ghost-btn" onClick={copyLink}>
          {copied ? <CheckIcon width={16} height={16} /> : <LinkIcon width={16} height={16} />}
          {copied ? 'Link copied' : 'Copy link'}
        </button>
      </header>

      <div className="prejoin-body">
        <section className="preview-pane">
          <div className={`preview ${camOn ? '' : 'camoff'}`}>
            {camOn ? (
              <video ref={videoRef} autoPlay playsInline muted className="preview-video" />
            ) : (
              <div className="preview-off">
                <span className="preview-avatar">{(name || 'You').slice(0, 1).toUpperCase()}</span>
                <span className="preview-off-label">Camera is off</span>
              </div>
            )}
            <div className="preview-controls">
              <button
                type="button"
                className={`round ${micOn ? '' : 'off'}`}
                onClick={() => setMicOn((v) => !v)}
                aria-pressed={micOn}
                aria-label={micOn ? 'Turn off microphone' : 'Turn on microphone'}
              >
                {micOn ? <MicIcon /> : <MicOffIcon />}
              </button>
              <button
                type="button"
                className={`round ${camOn ? '' : 'off'}`}
                onClick={() => setCamOn((v) => !v)}
                aria-pressed={camOn}
                aria-label={camOn ? 'Turn off camera' : 'Turn on camera'}
              >
                {camOn ? <CamIcon /> : <CamOffIcon />}
              </button>
            </div>
          </div>

          <div className="prejoin-devices">
            <DeviceSelect label="Camera" icon={<CamIcon width={16} height={16} />} value={camId} options={devices.cameras} onChange={setCamId} />
            <DeviceSelect label="Microphone" icon={<MicIcon width={16} height={16} />} value={micId} options={devices.mics} onChange={setMicId} />
          </div>
          {permError && <p className="prejoin-warn">{permError}</p>}
        </section>

        <section className="join-pane">
          <h1 className="join-title">Ready to join?</h1>
          <p className="join-room">
            {room ? (
              <>
                Room <code>{room}</code>
              </>
            ) : (
              'Enter a room to continue'
            )}
          </p>

          <label className="field">
            <span className="field-label">Your name</span>
            <input
              type="text"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="e.g. Amara Ndlovu"
              maxLength={64}
              autoFocus
            />
          </label>

          {!config.room && (
            <label className="field">
              <span className="field-label">Room</span>
              <input
                type="text"
                value={room}
                onChange={(e) => setRoom(e.target.value)}
                placeholder="tenant:room-name"
              />
            </label>
          )}

          {!hasTokenInUrl && !config.demo && (
            <label className="field">
              <span className="field-label">Access token</span>
              <textarea
                value={token}
                onChange={(e) => setToken(e.target.value)}
                placeholder="Paste your VULOS-MEET/1 token"
                rows={3}
                spellCheck={false}
              />
              <span className="field-hint">
                Meeting tokens are minted upstream (Vulos Workspace, Talk, or a meeting link) — Meet never mints them. For
                local dev, generate one with <code>npm run mint-dev-token</code>.
              </span>
            </label>
          )}

          <button type="button" className="btn primary lg" disabled={!canJoin} onClick={join}>
            Join meeting
          </button>
          <p className="join-foot">Your camera and mic preview stay on this device until you join.</p>
        </section>
      </div>
    </div>
  )
}
