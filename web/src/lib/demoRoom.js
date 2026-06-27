// DemoRoom — an offline, deterministic call controller used by the Playwright
// screenshotter and for local UI work without a real SFU, camera, or token.
// It emits the exact same snapshot shape as LiveRoom, so the React UI renders
// real chrome over seeded state. No media devices or network are touched.
//
// A demo "video" tile carries a descriptor track ({ demo:true, ... }) instead
// of a live MediaStreamTrack; VideoTile renders a styled placeholder for it.

const PEOPLE = [
  { id: 'amara', name: 'Amara Ndlovu', hue: 280, camOn: true, micOn: true, speaking: true },
  { id: 'sipho', name: 'Sipho Khumalo', hue: 210, camOn: true, micOn: true, speaking: false },
  { id: 'lena', name: 'Lena Fischer', hue: 150, camOn: false, micOn: false, speaking: false },
  { id: 'tariq', name: 'Tariq Bashir', hue: 25, camOn: true, micOn: true, speaking: false, handRaised: true },
  { id: 'mei', name: 'Mei Tanaka', hue: 330, camOn: true, micOn: false, speaking: false },
]

const ME = { id: 'you', name: 'You', hue: 195, camOn: true, micOn: true }

const MESSAGES = [
  { from: 'Amara Ndlovu', text: "Morning all — let's keep this to 20 minutes.", min: 6 },
  { from: 'Sipho Khumalo', text: 'Sharing my screen with the latency numbers now.', min: 4 },
  { from: 'Lena Fischer', text: 'The za-jhb box is holding steady at 38ms p50.', min: 3 },
  { from: 'You', text: 'Nice. Cascading to eu-fra worked on the last test.', min: 1, self: true },
]

function demoTrack(kind, hue, label) {
  return { demo: true, kind, hue, label }
}

function buildPeople({ withScreen = false } = {}) {
  const participants = []
  const tiles = []
  let presenter = null

  const push = (p, isLocal) => {
    participants.push({
      id: p.id,
      name: p.name,
      isLocal,
      micOn: !!p.micOn,
      camOn: !!p.camOn,
      screenOn: false,
      speaking: !!p.speaking,
      handRaised: !!p.handRaised,
    })
    tiles.push({
      key: `${p.id}:cam`,
      name: isLocal ? `${p.name}` : p.name,
      isLocal,
      micOn: !!p.micOn,
      camOn: !!p.camOn,
      speaking: !!p.speaking,
      handRaised: !!p.handRaised,
      kind: 'camera',
      track: p.camOn ? demoTrack('camera', p.hue, p.name) : null,
    })
  }

  push(ME, true)
  for (const p of PEOPLE) push(p, false)

  if (withScreen) {
    const presenterPerson = PEOPLE[1] // Sipho
    presenter = `${presenterPerson.id}:screen`
    tiles.unshift({
      key: presenter,
      name: `${presenterPerson.name} — screen`,
      isLocal: false,
      micOn: true,
      camOn: true,
      speaking: false,
      handRaised: false,
      kind: 'screen',
      track: demoTrack('screen', 210, 'Latency dashboard'),
    })
    const sp = participants.find((x) => x.id === presenterPerson.id)
    if (sp) sp.screenOn = true
  }

  return { participants, tiles, presenter }
}

function demoMessages() {
  const now = Date.now()
  return MESSAGES.map((m, i) => ({
    id: `m${i}`,
    from: m.from,
    text: m.text,
    ts: now - m.min * 60_000,
    self: !!m.self,
  }))
}

export class DemoRoom {
  constructor(scene = 'in-room') {
    this.scene = scene
    this.listeners = new Set()
    this.state = this._sceneState(scene)
  }

  _sceneState(scene) {
    const roomName = 'standup-2026-06-27'
    const local = { id: ME.id, name: ME.name, micOn: true, camOn: true, screenOn: scene === 'screen-share', handRaised: false }
    const messages = demoMessages()

    if (scene === 'connecting') {
      return { status: 'connecting', error: null, room: { name: roomName }, tiles: [], participants: [], presenter: null, local, messages: [] }
    }
    if (scene === 'reconnecting') {
      const base = buildPeople()
      return { status: 'reconnecting', error: null, room: { name: roomName }, ...base, local, messages }
    }
    if (scene === 'ended') {
      return { status: 'ended', error: null, room: { name: roomName }, tiles: [], participants: [], presenter: null, local, messages }
    }
    if (scene === 'error') {
      return { status: 'error', error: 'Could not reach the meeting server. Check the connection and try again.', room: { name: roomName }, tiles: [], participants: [], presenter: null, local, messages: [] }
    }
    if (scene === 'permission-denied') {
      return { status: 'permission-denied', error: null, room: { name: roomName }, tiles: [], participants: [], presenter: null, local, messages: [] }
    }

    const withScreen = scene === 'screen-share'
    const base = buildPeople({ withScreen })
    return { status: 'connected', error: null, room: { name: roomName }, ...base, local, messages }
  }

  on(cb) {
    this.listeners.add(cb)
    cb(this.state)
    return () => this.listeners.delete(cb)
  }

  _emit() {
    for (const cb of this.listeners) cb(this.state)
  }

  async connect() {
    /* already seeded */
  }

  _patchLocal(patch) {
    this.state = { ...this.state, local: { ...this.state.local, ...patch } }
    // Reflect into the local tile so toggles are visible.
    this.state.tiles = this.state.tiles.map((t) =>
      t.isLocal && t.kind === 'camera'
        ? { ...t, micOn: this.state.local.micOn, camOn: this.state.local.camOn, handRaised: this.state.local.handRaised, track: this.state.local.camOn ? demoTrack('camera', ME.hue, ME.name) : null }
        : t,
    )
    this._emit()
  }

  toggleMic() {
    this._patchLocal({ micOn: !this.state.local.micOn })
  }
  toggleCam() {
    this._patchLocal({ camOn: !this.state.local.camOn })
  }
  toggleScreenShare() {
    this._patchLocal({ screenOn: !this.state.local.screenOn })
  }
  toggleHand() {
    this._patchLocal({ handRaised: !this.state.local.handRaised })
  }
  switchDevice() {}

  sendChat(text) {
    const t = String(text || '').trim()
    if (!t) return
    this.state = {
      ...this.state,
      messages: [...this.state.messages, { id: `s${Date.now()}`, from: 'You', text: t, ts: Date.now(), self: true }],
    }
    this._emit()
  }

  // Whiteboard has no peers in the offline demo — board edits stay local.
  onBoardData() {
    return () => {}
  }
  publishBoardData() {}

  async leave() {
    this.state = { ...this.state, status: 'left' }
    this._emit()
  }
}
