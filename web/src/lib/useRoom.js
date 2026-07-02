import { useEffect, useRef, useState, useCallback } from 'react'
import { LiveRoom } from './liveRoom.js'
import { DemoRoom } from './demoRoom.js'

// useRoom owns the call controller (real LiveKit or offline demo) and exposes a
// reactive snapshot plus stable action callbacks. The UI never branches on
// real-vs-demo: both controllers emit the same snapshot shape.
export function useRoom() {
  const ctrlRef = useRef(null)
  const [snapshot, setSnapshot] = useState(null)

  useEffect(() => () => ctrlRef.current?.leave?.(), [])

  const connectLive = useCallback((opts) => {
    const c = new LiveRoom()
    ctrlRef.current = c
    c.on(setSnapshot)
    c.connect(opts)
    return c
  }, [])

  const startDemo = useCallback((scene) => {
    const c = new DemoRoom(scene)
    ctrlRef.current = c
    c.on(setSnapshot)
    return c
  }, [])

  const actions = {
    toggleMic: useCallback(() => ctrlRef.current?.toggleMic(), []),
    toggleCam: useCallback(() => ctrlRef.current?.toggleCam(), []),
    toggleScreenShare: useCallback(() => ctrlRef.current?.toggleScreenShare(), []),
    toggleHand: useCallback(() => ctrlRef.current?.toggleHand(), []),
    switchDevice: useCallback((kind, id) => ctrlRef.current?.switchDevice(kind, id), []),
    sendChat: useCallback((text) => ctrlRef.current?.sendChat(text), []),
    sendReaction: useCallback((emoji) => ctrlRef.current?.sendReaction(emoji), []),
    onReaction: useCallback((cb) => ctrlRef.current?.onReaction?.(cb) ?? (() => {}), []),
    publishBoardData: useCallback((bytes, topic) => ctrlRef.current?.publishBoardData(bytes, topic), []),
    onBoardData: useCallback((cb) => ctrlRef.current?.onBoardData?.(cb) ?? (() => {}), []),
    leave: useCallback(() => ctrlRef.current?.leave(), []),
  }

  return { snapshot, connectLive, startDemo, actions }
}
