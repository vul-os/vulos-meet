import { useEffect, useRef, useState } from 'react'
import {
  MicIcon,
  MicOffIcon,
  CamIcon,
  CamOffIcon,
  ScreenIcon,
  HandIcon,
  LeaveIcon,
  ChatIcon,
  PeopleIcon,
  SettingsIcon,
  BoardIcon,
  ChevronDownIcon,
} from './Icons.jsx'

function ControlButton({ label, active, danger, on = true, badge, onClick, children, extra }) {
  return (
    <div className="ctrl-wrap">
      <button
        type="button"
        className={`ctrl ${danger ? 'danger' : ''} ${active ? 'active' : ''} ${!on ? 'off' : ''}`}
        onClick={onClick}
        aria-pressed={active ? true : undefined}
        aria-label={label}
        title={label}
      >
        {children}
        {badge != null && badge > 0 ? <span className="ctrl-badge">{badge}</span> : null}
      </button>
      <span className="ctrl-label">{label}</span>
      {extra}
    </div>
  )
}

function DeviceMenu({ devices, onPick, onClose }) {
  const ref = useRef(null)
  useEffect(() => {
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
  }, [onClose])

  const group = (title, kind, list) =>
    list && list.length ? (
      <div className="dm-group">
        <div className="dm-title">{title}</div>
        {list.map((d) => (
          <button key={d.deviceId} type="button" className="dm-item" onClick={() => onPick(kind, d.deviceId)}>
            {d.label}
          </button>
        ))}
      </div>
    ) : null

  return (
    <div className="device-menu" ref={ref} role="menu" aria-label="Devices">
      {group('Camera', 'camera', devices.cameras)}
      {group('Microphone', 'mic', devices.mics)}
      {group('Speaker', 'speaker', devices.speakers)}
      {!devices.cameras?.length && !devices.mics?.length && (
        <div className="dm-empty">No devices detected.</div>
      )}
    </div>
  )
}

export default function ControlBar({ local, actions, devices, panel, setPanel, participantCount, unread }) {
  const [menu, setMenu] = useState(false)

  return (
    <div className="control-bar" role="toolbar" aria-label="Meeting controls">
      <ControlButton
        label={local.micOn ? 'Mute' : 'Unmute'}
        active={local.micOn}
        on={local.micOn}
        onClick={actions.toggleMic}
        extra={
          menu && (
            <DeviceMenu
              devices={devices}
              onPick={(kind, id) => {
                actions.switchDevice(kind, id)
                setMenu(false)
              }}
              onClose={() => setMenu(false)}
            />
          )
        }
      >
        {local.micOn ? <MicIcon /> : <MicOffIcon />}
        <button
          type="button"
          className="ctrl-caret"
          aria-label="Choose devices"
          title="Choose devices"
          onClick={(e) => {
            e.stopPropagation()
            setMenu((v) => !v)
          }}
        >
          <ChevronDownIcon width={14} height={14} />
        </button>
      </ControlButton>

      <ControlButton label={local.camOn ? 'Stop video' : 'Start video'} active={local.camOn} on={local.camOn} onClick={actions.toggleCam}>
        {local.camOn ? <CamIcon /> : <CamOffIcon />}
      </ControlButton>

      <ControlButton label={local.screenOn ? 'Stop sharing' : 'Share screen'} active={local.screenOn} onClick={actions.toggleScreenShare}>
        <ScreenIcon />
      </ControlButton>

      <ControlButton label={local.handRaised ? 'Lower hand' : 'Raise hand'} active={local.handRaised} onClick={actions.toggleHand}>
        <HandIcon />
      </ControlButton>

      <div className="ctrl-sep" aria-hidden />

      <ControlButton
        label="Participants"
        active={panel === 'people'}
        badge={participantCount}
        onClick={() => setPanel(panel === 'people' ? null : 'people')}
      >
        <PeopleIcon />
      </ControlButton>

      <ControlButton
        label="Chat"
        active={panel === 'chat'}
        badge={panel !== 'chat' ? unread : 0}
        onClick={() => setPanel(panel === 'chat' ? null : 'chat')}
      >
        <ChatIcon />
      </ControlButton>

      <ControlButton
        label="Whiteboard"
        active={panel === 'whiteboard'}
        onClick={() => setPanel(panel === 'whiteboard' ? null : 'whiteboard')}
      >
        <BoardIcon />
      </ControlButton>

      <ControlButton label="Settings" active={panel === 'settings'} onClick={() => setPanel(panel === 'settings' ? null : 'settings')}>
        <SettingsIcon />
      </ControlButton>

      <div className="ctrl-sep" aria-hidden />

      <ControlButton label="Leave" danger onClick={actions.leave}>
        <LeaveIcon />
      </ControlButton>
    </div>
  )
}
