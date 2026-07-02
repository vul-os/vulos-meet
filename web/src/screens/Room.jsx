import { lazy, Suspense, useCallback, useEffect, useRef, useState } from 'react'
import VideoGrid from '../components/VideoGrid.jsx'
import ControlBar from '../components/ControlBar.jsx'
import ParticipantsPanel from '../components/ParticipantsPanel.jsx'
import ChatPanel from '../components/ChatPanel.jsx'
import SettingsPanel from '../components/SettingsPanel.jsx'
import ShortcutsHelp from '../components/ShortcutsHelp.jsx'
import { ReactionsOverlay } from '../components/Reactions.jsx'
import { LogoMark } from '../components/Logo.jsx'
import { MicOffIcon } from '../components/Icons.jsx'
import { resolveShortcut, isPushToTalkKey } from '../lib/shortcuts.js'

// Excalidraw + the Yjs board stack is heavy (~MBs). Load it as a separate async
// chunk only when the whiteboard is first opened — not on call join.
const WhiteboardPanel = lazy(() => import('../components/WhiteboardPanel.jsx'))

export default function Room({ snapshot, actions, devices, selectedDevices, onSelectDevice, theme, onTheme }) {
  const [panel, setPanel] = useState(null)
  const [unread, setUnread] = useState(0)
  const [pinned, setPinned] = useState(null)
  const [reactionsOpen, setReactionsOpen] = useState(false)
  const [showHelp, setShowHelp] = useState(false)
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

  const togglePin = useCallback((key) => setPinned((cur) => (cur === key ? null : key)), [])
  const togglePanel = useCallback((name) => setPanel((cur) => (cur === name ? null : name)), [])

  // ---- keyboard shortcuts + push-to-talk ----
  // PTT: hold Space to talk while muted; we unmute on keydown and re-mute on
  // keyup, but only when the mic started muted (so we never mute a live mic).
  const pttActive = useRef(false)
  const micOn = snapshot.local.micOn
  const micRef = useRef(micOn)
  micRef.current = micOn

  useEffect(() => {
    const onKeyDown = (e) => {
      if (isPushToTalkKey(e)) {
        if (e.repeat) return
        if (!micRef.current && !pttActive.current) {
          pttActive.current = true
          e.preventDefault()
          actions.toggleMic()
        }
        return
      }
      const action = resolveShortcut(e)
      if (!action) return
      e.preventDefault()
      switch (action) {
        case 'mic':
          actions.toggleMic()
          break
        case 'cam':
          actions.toggleCam()
          break
        case 'screen':
          actions.toggleScreenShare()
          break
        case 'hand':
          actions.toggleHand()
          break
        case 'chat':
          togglePanel('chat')
          break
        case 'people':
          togglePanel('people')
          break
        case 'reactions':
          setReactionsOpen((v) => !v)
          break
        case 'help':
          setShowHelp((v) => !v)
          break
        default:
          break
      }
    }
    const onKeyUp = (e) => {
      if (isPushToTalkKey(e) && pttActive.current) {
        pttActive.current = false
        // Re-mute if PTT put us live (guard against a manual toggle mid-hold).
        if (micRef.current) actions.toggleMic()
      }
    }
    window.addEventListener('keydown', onKeyDown)
    window.addEventListener('keyup', onKeyUp)
    return () => {
      window.removeEventListener('keydown', onKeyDown)
      window.removeEventListener('keyup', onKeyUp)
    }
  }, [actions, togglePanel])

  const reconnecting = snapshot.status === 'reconnecting'
  const mutedButTalking = snapshot.local.mutedButTalking

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
          <VideoGrid tiles={snapshot.tiles} presenter={snapshot.presenter} pinned={pinned} onTogglePin={togglePin} />
          <ReactionsOverlay actions={actions} />
          {mutedButTalking && (
            <div className="muted-hint" role="status">
              <MicOffIcon width={16} height={16} />
              <span>
                You’re muted — press <kbd>M</kbd> to unmute, or hold <kbd>Space</kbd> to talk
              </span>
            </div>
          )}
        </div>

        {panel === 'people' && <ParticipantsPanel participants={snapshot.participants} onClose={() => setPanel(null)} />}
        {panel === 'chat' && (
          <ChatPanel
            messages={snapshot.messages}
            onSend={actions.sendChat}
            onClose={() => setPanel(null)}
            synced={snapshot.chat?.synced}
          />
        )}
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
          <Suspense
            fallback={
              <aside className="panel panel-board" aria-label="Whiteboard" aria-busy="true">
                <div className="board-loading" role="status">Loading whiteboard…</div>
              </aside>
            }
          >
            <WhiteboardPanel
              user={{ id: snapshot.local.id, name: snapshot.local.name }}
              roomId={snapshot.room.id || snapshot.room.name}
              actions={actions}
              onClose={() => setPanel(null)}
            />
          </Suspense>
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
        reactionsOpen={reactionsOpen}
        setReactionsOpen={setReactionsOpen}
      />

      {showHelp && <ShortcutsHelp onClose={() => setShowHelp(false)} />}
    </div>
  )
}
