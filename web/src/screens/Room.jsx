import { useEffect, useRef, useState } from 'react'
import VideoGrid from '../components/VideoGrid.jsx'
import ControlBar from '../components/ControlBar.jsx'
import ParticipantsPanel from '../components/ParticipantsPanel.jsx'
import ChatPanel from '../components/ChatPanel.jsx'
import SettingsPanel from '../components/SettingsPanel.jsx'
import WhiteboardPanel from '../components/WhiteboardPanel.jsx'
import { LogoMark } from '../components/Logo.jsx'

export default function Room({ snapshot, actions, devices, selectedDevices, onSelectDevice, theme, onTheme }) {
  const [panel, setPanel] = useState(null)
  const [unread, setUnread] = useState(0)
  const lastCount = useRef(snapshot.messages.length)

  // Track unread chat while the chat panel is closed.
  useEffect(() => {
    const n = snapshot.messages.length
    if (panel === 'chat') {
      setUnread(0)
      lastCount.current = n
      return
    }
    const delta = n - lastCount.current
    if (delta > 0) setUnread((u) => u + delta)
    lastCount.current = n
  }, [snapshot.messages.length, panel])

  const reconnecting = snapshot.status === 'reconnecting'

  return (
    <div className={`room ${panel ? 'with-panel' : ''}`}>
      <header className="room-head">
        <div className="room-id">
          <span className="room-mark">
            <LogoMark size={20} />
          </span>
          <span className="room-name" title={snapshot.room.name}>
            {snapshot.room.name || 'Meeting'}
          </span>
          <span className="room-people">
            {snapshot.participants.length} {snapshot.participants.length === 1 ? 'person' : 'people'}
          </span>
        </div>
        {reconnecting && (
          <div className="room-banner" role="status">
            <span className="dot-pulse" aria-hidden /> Reconnecting…
          </div>
        )}
      </header>

      <div className="room-main">
        <div className="stage-wrap">
          <VideoGrid tiles={snapshot.tiles} presenter={snapshot.presenter} />
        </div>

        {panel === 'people' && <ParticipantsPanel participants={snapshot.participants} onClose={() => setPanel(null)} />}
        {panel === 'chat' && <ChatPanel messages={snapshot.messages} onSend={actions.sendChat} onClose={() => setPanel(null)} />}
        {panel === 'settings' && (
          <SettingsPanel
            devices={devices}
            selected={selectedDevices}
            onSelect={onSelectDevice}
            theme={theme}
            onTheme={onTheme}
            onClose={() => setPanel(null)}
          />
        )}
        {panel === 'whiteboard' && (
          <WhiteboardPanel
            user={{ id: snapshot.local.id, name: snapshot.local.name }}
            roomId={snapshot.room.id || snapshot.room.name}
            actions={actions}
            onClose={() => setPanel(null)}
          />
        )}
      </div>

      <ControlBar
        local={snapshot.local}
        actions={actions}
        devices={devices}
        panel={panel}
        setPanel={setPanel}
        participantCount={snapshot.participants.length}
        unread={unread}
      />
    </div>
  )
}
