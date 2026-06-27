import { useEffect, useMemo, useState, useCallback } from 'react'
import PreJoin from './screens/PreJoin.jsx'
import Room from './screens/Room.jsx'
import AppsAndBotsView from './screens/AppsAndBotsView.jsx'
import StatusScreen from './components/StatusScreen.jsx'
import { parseConfig, roomDisplayName, talkBinding } from './lib/config.js'
import { useRoom } from './lib/useRoom.js'
import { enumerate } from './lib/devices.js'

function useTheme() {
  const [theme, setTheme] = useState(() => localStorage.getItem('vulos-meet-theme') || 'dark')
  useEffect(() => {
    document.documentElement.setAttribute('data-theme', theme)
    localStorage.setItem('vulos-meet-theme', theme)
  }, [theme])
  return [theme, setTheme]
}

export default function App() {
  const config = useMemo(() => parseConfig(), [])
  const { snapshot, connectLive, startDemo, actions } = useRoom()
  const [phase, setPhase] = useState(config.demo && config.demo !== 'prejoin' ? 'room' : 'prejoin')
  const [theme, setTheme] = useTheme()
  const [devices, setDevices] = useState({ cameras: [], mics: [], speakers: [] })
  const [selectedDevices, setSelectedDevices] = useState({ camera: '', mic: '', speaker: '' })

  // Demo mode: skip the lobby and seed the room controller directly.
  useEffect(() => {
    if (config.demo && config.demo !== 'prejoin') {
      const scene = config.demo === '1' || config.demo === 'true' ? 'in-room' : config.demo
      startDemo(scene)
    }
  }, [config.demo, startDemo])

  useEffect(() => {
    enumerate().then(setDevices).catch(() => {})
  }, [phase])

  const handleJoin = useCallback(
    (opts) => {
      setSelectedDevices({ camera: opts.cameraId, mic: opts.micId, speaker: '' })
      setPhase('room')
      connectLive({
        serverUrl: config.serverUrl,
        token: opts.token,
        displayName: opts.displayName,
        cameraId: opts.cameraId,
        micId: opts.micId,
        cam: opts.cam,
        mic: opts.mic,
        roomName: roomDisplayName(opts.room || config.room, config.separator),
        talk: talkBinding(config),
      })
    },
    [config, connectLive],
  )

  const onSelectDevice = useCallback(
    (kind, id) => {
      setSelectedDevices((s) => ({ ...s, [kind]: id }))
      actions.switchDevice(kind, id)
    },
    [actions],
  )

  const rejoin = useCallback(() => {
    setPhase('prejoin')
    window.location.reload()
  }, [])

  // ---- render ----
  // Apps & Bots management place (/apps or ?view=apps) — a separate surface
  // from the call UI, over the shared @vulos/apps platform at /api/apps.
  if (config.view === 'apps') {
    return <AppsAndBotsView theme={theme} onTheme={setTheme} />
  }

  if (phase === 'prejoin') {
    return <PreJoin config={config} onJoin={handleJoin} />
  }

  const status = snapshot?.status || 'connecting'

  // Full-screen state surfaces take over for non-connected states.
  if (status === 'connecting' || status === 'error' || status === 'permission-denied' || status === 'left' || status === 'ended') {
    return (
      <StatusScreen
        status={status}
        message={snapshot?.error}
        onRetry={rejoin}
        onRejoin={rejoin}
      />
    )
  }

  // connected OR reconnecting (reconnecting shows an in-room banner).
  return (
    <Room
      snapshot={snapshot}
      actions={actions}
      devices={devices}
      selectedDevices={selectedDevices}
      onSelectDevice={onSelectDevice}
      theme={theme}
      onTheme={setTheme}
    />
  )
}
