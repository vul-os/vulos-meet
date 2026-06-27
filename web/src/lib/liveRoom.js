// LiveRoom — the real LiveKit-backed call controller.
//
// Wraps livekit-client's Room and exposes a small, normalised surface the React
// UI consumes: a snapshot (status, tiles, participants, chat, local state) and a
// set of actions (toggle mic/cam/screen/hand, send chat, leave). The same shape
// is produced by DemoRoom, so the UI never branches on real-vs-demo.
//
// Connection target: the LiveKit signal URL (defaults to this origin — the
// vulos-meet signal gate fronts /rtc) and a VULOS-MEET/1 token minted upstream.
// LiveKit re-verifies that token on /rtc; the gate verified it first. The client
// only consumes it.

import {
  Room,
  RoomEvent,
  Track,
  ConnectionState,
  DisconnectReason,
} from 'livekit-client'
import { EphemeralChatTransport, createChatTransport } from './chatTransport.js'

export class LiveRoom {
  constructor() {
    this.room = null
    this.listeners = new Set()
    this.boardListeners = new Set()
    this.messages = []
    this.status = 'idle'
    this.error = null
    this.roomName = ''
    this._audioEls = new Map() // trackSid -> HTMLAudioElement

    // In-call chat is delegated to a pluggable ChatTransport. We start on the
    // ephemeral (data-channel) transport so chat works pre-connect and for
    // standalone meetings; connect() may swap in a Talk-backed transport.
    this._chatDataListeners = new Set()
    this._chatSynced = false
    this._chatLabel = 'ephemeral'
    this._chatUnsub = null
    this.chat = null
    this._swapChat(new EphemeralChatTransport(this._chatWire()))
  }

  // The seam the ephemeral transport rides: publish a chat envelope on the
  // dedicated 'chat' data topic, and receive inbound chat envelopes. Board
  // traffic uses separate topics so the two never collide.
  _chatWire() {
    return {
      publishChat: (text) => {
        if (!this.room) return
        const payload = new TextEncoder().encode(JSON.stringify({ kind: 'chat', text }))
        this.room.localParticipant.publishData(payload, { reliable: true, topic: 'chat' }).catch(() => {})
      },
      onChatData: (cb) => {
        this._chatDataListeners.add(cb)
        return () => this._chatDataListeners.delete(cb)
      },
    }
  }

  _swapChat(transport) {
    this._chatUnsub?.()
    this.chat?.dispose?.()
    this.chat = transport
    this._chatSynced = !!transport.synced
    this._chatLabel = transport.label || 'ephemeral'
    this._chatUnsub = transport.subscribe((messages) => {
      this.messages = messages
      this._chatSynced = !!transport.synced
      this._chatLabel = transport.label || 'ephemeral'
      this._emit()
    })
  }

  // Decide the chat transport once connected: Talk binding present + reachable ->
  // Talk-backed (persistent); else keep the ephemeral data-channel transport.
  async _setupChat(talk, displayName) {
    const binding = talk ? { ...talk, self: talk.self || displayName || '' } : null
    const next = await createChatTransport({
      talk: binding,
      wire: this._chatWire(),
      fetchImpl: typeof fetch !== 'undefined' ? fetch.bind(globalThis) : undefined,
    })
    this._swapChat(next)
  }

  on(cb) {
    this.listeners.add(cb)
    cb(this.snapshot())
    return () => this.listeners.delete(cb)
  }

  _emit() {
    const snap = this.snapshot()
    for (const cb of this.listeners) cb(snap)
  }

  async connect({ serverUrl, token, displayName, cameraId, micId, mic = true, cam = true, roomName = '', talk = null }) {
    this.status = 'connecting'
    this.roomName = roomName
    this._emit()

    const room = new Room({
      adaptiveStream: true,
      dynacast: true,
      videoCaptureDefaults: cameraId ? { deviceId: cameraId } : undefined,
      audioCaptureDefaults: micId ? { deviceId: micId } : undefined,
    })
    this.room = room

    room
      .on(RoomEvent.ParticipantConnected, () => this._emit())
      .on(RoomEvent.ParticipantDisconnected, () => this._emit())
      .on(RoomEvent.TrackSubscribed, (track) => {
        if (track.kind === Track.Kind.Audio) this._attachAudio(track)
        this._emit()
      })
      .on(RoomEvent.TrackUnsubscribed, (track) => {
        if (track.kind === Track.Kind.Audio) this._detachAudio(track)
        this._emit()
      })
      .on(RoomEvent.LocalTrackPublished, () => this._emit())
      .on(RoomEvent.LocalTrackUnpublished, () => this._emit())
      .on(RoomEvent.TrackMuted, () => this._emit())
      .on(RoomEvent.TrackUnmuted, () => this._emit())
      .on(RoomEvent.ActiveSpeakersChanged, () => this._emit())
      .on(RoomEvent.ParticipantAttributesChanged, () => this._emit())
      .on(RoomEvent.DataReceived, (payload, participant, _kind, topic) => this._onData(payload, participant, topic))
      .on(RoomEvent.ConnectionStateChanged, (s) => this._onConnState(s))
      .on(RoomEvent.Reconnecting, () => {
        this.status = 'reconnecting'
        this._emit()
      })
      .on(RoomEvent.Reconnected, () => {
        this.status = 'connected'
        this._emit()
      })
      .on(RoomEvent.Disconnected, (reason) => this._onDisconnected(reason))

    try {
      await room.connect(serverUrl, token)
      if (displayName) await room.localParticipant.setName(displayName)
      this.status = 'connected'
      this._emit()
      // Pick the chat transport now the join context is known. Talk-bound
      // meetings get persistent chat; everything else stays ephemeral.
      await this._setupChat(talk, displayName).catch(() => {})
      // Enable media after connect so a permission denial surfaces in-room
      // rather than silently dropping the join.
      if (cam) await room.localParticipant.setCameraEnabled(true).catch((e) => this._mediaErr(e))
      if (mic) await room.localParticipant.setMicrophoneEnabled(true).catch((e) => this._mediaErr(e))
      this._emit()
    } catch (err) {
      this.status = 'error'
      this.error = friendlyError(err)
      this._emit()
    }
  }

  _mediaErr(err) {
    if (err?.name === 'NotAllowedError') {
      this.status = 'permission-denied'
      this._emit()
    }
  }

  _onConnState(s) {
    if (s === ConnectionState.Connected && this.status !== 'connected') {
      this.status = 'connected'
      this._emit()
    }
  }

  _onDisconnected(reason) {
    // Distinguish a deliberate leave from a server/room termination.
    if (reason === DisconnectReason.CLIENT_INITIATED) this.status = 'left'
    else if (reason === DisconnectReason.ROOM_DELETED || reason === DisconnectReason.PARTICIPANT_REMOVED)
      this.status = 'ended'
    else this.status = 'left'
    this._emit()
  }

  _attachAudio(track) {
    const el = track.attach()
    el.style.display = 'none'
    document.body.appendChild(el)
    this._audioEls.set(track.sid, el)
  }

  _detachAudio(track) {
    const el = this._audioEls.get(track.sid)
    if (el) {
      track.detach(el)
      el.remove()
      this._audioEls.delete(track.sid)
    }
  }

  _onData(payload, participant, topic) {
    // Board traffic rides the same data channel under dedicated topics and is
    // raw Yjs/awareness bytes (NOT JSON) — dispatch it to board listeners and
    // stop so it never reaches chat's JSON parser.
    if (topic === 'board' || topic === 'board-aware' || topic === 'board-ctl') {
      for (const cb of this.boardListeners) cb(topic, payload)
      return
    }
    try {
      const msg = JSON.parse(new TextDecoder().decode(payload))
      if (msg.kind === 'chat') {
        // Hand inbound chat to whatever transport is active. The ephemeral
        // transport ingests it; the Talk transport ignores the data channel
        // (its source of truth is the Talk channel) so duplicates never appear.
        const from = participant?.name || participant?.identity || 'Guest'
        if (!this._chatSynced) for (const cb of this._chatDataListeners) cb(from, String(msg.text || ''))
      }
    } catch {
      /* ignore malformed data */
    }
  }

  // ---- whiteboard data channel ----
  // The board panel writes a small Yjs provider on top of these. Board updates
  // are published as raw Uint8Array under a dedicated LiveKit data `topic` so
  // they never collide with chat's JSON envelope. `topic` is one of: 'board'
  // (Yjs document updates / full-state replies), 'board-aware' (awareness /
  // remote-cursor updates) or 'board-ctl' (late-join hello).
  onBoardData(cb) {
    this.boardListeners.add(cb)
    return () => this.boardListeners.delete(cb)
  }

  publishBoardData(bytes, topic = 'board') {
    if (!this.room) return
    this.room.localParticipant.publishData(bytes, { reliable: true, topic }).catch(() => {})
  }

  // ---- actions ----
  async toggleMic() {
    const lp = this.room?.localParticipant
    if (!lp) return
    await lp.setMicrophoneEnabled(!lp.isMicrophoneEnabled).catch((e) => this._mediaErr(e))
    this._emit()
  }

  async toggleCam() {
    const lp = this.room?.localParticipant
    if (!lp) return
    await lp.setCameraEnabled(!lp.isCameraEnabled).catch((e) => this._mediaErr(e))
    this._emit()
  }

  async toggleScreenShare() {
    const lp = this.room?.localParticipant
    if (!lp) return
    const on = lp.isScreenShareEnabled
    await lp.setScreenShareEnabled(!on).catch(() => {})
    this._emit()
  }

  async toggleHand() {
    const lp = this.room?.localParticipant
    if (!lp) return
    const raised = lp.attributes?.handRaised === 'true'
    await lp.setAttributes({ ...lp.attributes, handRaised: raised ? '' : 'true' }).catch(() => {})
    this._emit()
  }

  async switchDevice(kind, deviceId) {
    if (!this.room) return
    const map = { camera: 'videoinput', mic: 'audioinput', speaker: 'audiooutput' }
    await this.room.switchActiveDevice(map[kind] || kind, deviceId).catch(() => {})
    this._emit()
  }

  sendChat(text) {
    this.chat?.send?.(text)
  }

  async leave() {
    this.chat?.dispose?.()
    if (this.room) await this.room.disconnect()
    this.status = 'left'
    this._emit()
  }

  // ---- snapshot ----
  snapshot() {
    const room = this.room
    if (!room) {
      return baseSnapshot(this.status, this.error, this.roomName, this.messages)
    }
    const speaking = new Set(room.activeSpeakers.map((p) => p.identity))
    const participants = []
    const tiles = []
    let presenter = null

    const addFor = (p, isLocal) => {
      const camPub = p.getTrackPublication(Track.Source.Camera)
      const screenPub = p.getTrackPublication(Track.Source.ScreenShare)
      const micPub = p.getTrackPublication(Track.Source.Microphone)
      const micOn = !!micPub && !micPub.isMuted
      const camOn = !!camPub && !camPub.isMuted && !!camPub.track
      const handRaised = p.attributes?.handRaised === 'true'
      const isSpeaking = speaking.has(p.identity)
      const name = p.name || p.identity || 'Guest'

      participants.push({
        id: p.identity,
        name,
        isLocal,
        micOn,
        camOn,
        screenOn: !!screenPub && !!screenPub.track,
        speaking: isSpeaking,
        handRaised,
      })

      tiles.push({
        key: `${p.identity}:cam`,
        name,
        isLocal,
        micOn,
        camOn,
        speaking: isSpeaking,
        handRaised,
        kind: 'camera',
        track: camOn ? camPub.track : null,
      })

      if (screenPub && screenPub.track) {
        const key = `${p.identity}:screen`
        presenter = key
        tiles.push({
          key,
          name: `${name} — screen`,
          isLocal,
          micOn,
          camOn: true,
          speaking: false,
          handRaised: false,
          kind: 'screen',
          track: screenPub.track,
        })
      }
    }

    addFor(room.localParticipant, true)
    for (const p of room.remoteParticipants.values()) addFor(p, false)

    const lp = room.localParticipant
    return {
      status: this.status,
      error: this.error,
      room: { name: this.roomName || room.name, id: room.name },
      tiles,
      participants,
      presenter,
      local: {
        id: lp.identity,
        name: lp.name || lp.identity || 'You',
        micOn: lp.isMicrophoneEnabled,
        camOn: lp.isCameraEnabled,
        screenOn: lp.isScreenShareEnabled,
        handRaised: lp.attributes?.handRaised === 'true',
      },
      messages: this.messages,
      chat: { synced: this._chatSynced, label: this._chatLabel },
    }
  }
}

function baseSnapshot(status, error, roomName, messages) {
  return {
    status,
    error,
    room: { name: roomName, id: roomName },
    tiles: [],
    participants: [],
    presenter: null,
    local: { id: '', name: 'You', micOn: false, camOn: false, screenOn: false, handRaised: false },
    messages,
    chat: { synced: false, label: 'ephemeral' },
  }
}

function friendlyError(err) {
  const m = err?.message || String(err)
  if (/token/i.test(m)) return 'Could not join: the meeting token was rejected or has expired.'
  if (/network|connect|websocket/i.test(m)) return 'Could not reach the meeting server. Check the connection and try again.'
  return m
}
