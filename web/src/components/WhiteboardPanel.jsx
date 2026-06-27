import { useEffect, useMemo } from 'react'
import { BoardApp, createBoardDoc } from '@vulos/board-ui'
import * as Y from 'yjs'
import {
  Awareness,
  encodeAwarenessUpdate,
  applyAwarenessUpdate,
  removeAwarenessStates,
} from 'y-protocols/awareness'
import '@vulos/board-ui/style.css'
import { CloseIcon } from './Icons.jsx'

// Stable, readable cursor/selection tint derived from the participant identity
// so the same person keeps the same colour across reloads and peers.
function colorFor(id) {
  let h = 0
  const s = String(id || 'guest')
  for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) >>> 0
  return `hsl(${h % 360} 70% 55%)`
}

// In-call collaborative whiteboard.
//
// board-ui only ever reads/writes a plain Y.Doc; the HOST owns transport. Here
// the transport is the EXISTING LiveKit data channel (no extra server). We run
// a tiny Yjs provider over it across three dedicated topics:
//   - 'board'       document updates (CRDT) + full-state replies
//   - 'board-aware' presence/awareness updates (remote cursors + selection)
//   - 'board-ctl'   late-join hello sentinel
//
// Document:
//   - outbound: local doc updates -> publishBoardData(update, 'board')
//   - inbound:  'board' bytes -> Y.applyUpdate(doc, bytes, 'remote')
//
// Awareness (remote cursors / live identity colour):
//   - outbound: locally-originated awareness changes -> encodeAwarenessUpdate
//     -> publishBoardData(..., 'board-aware')
//   - inbound:  'board-aware' bytes -> applyAwarenessUpdate(awareness, bytes, 'remote')
//
// The 'remote' origin tag stops applied updates (doc AND awareness) from being
// re-broadcast, which prevents echo/rebroadcast loops.
//
// Late join: on mount we announce ourselves on the 'board-ctl' topic; any peer
// that already holds state replies with the full doc (encodeStateAsUpdate) on
// 'board' AND the full awareness snapshot on 'board-aware', so a newcomer sees
// both existing drawings and everyone's live cursors immediately. Applying full
// state is idempotent in Yjs/awareness, so duplicate replies are harmless.
export default function WhiteboardPanel({ user, roomId, actions, onClose }) {
  const ydoc = useMemo(() => createBoardDoc(), [])
  const awareness = useMemo(() => new Awareness(ydoc), [ydoc])
  const { publishBoardData, onBoardData } = actions

  const boardUser = useMemo(
    () => ({ id: user?.id || 'guest', name: user?.name || 'You', color: colorFor(user?.id) }),
    [user?.id, user?.name],
  )

  useEffect(() => {
    if (!publishBoardData || !onBoardData) return

    const onUpdate = (update, origin) => {
      if (origin !== 'remote') publishBoardData(update, 'board')
    }
    ydoc.on('update', onUpdate)

    // Only rebroadcast locally-originated awareness changes. Remote updates carry
    // the 'remote' origin (set below) so applying them never echoes back out.
    const onAware = ({ added, updated, removed }, origin) => {
      if (origin === 'remote') return
      const changed = added.concat(updated, removed)
      publishBoardData(encodeAwarenessUpdate(awareness, changed), 'board-aware')
    }
    awareness.on('update', onAware)

    const unsub = onBoardData((topic, bytes) => {
      if (topic === 'board') {
        Y.applyUpdate(ydoc, bytes, 'remote')
      } else if (topic === 'board-aware') {
        applyAwarenessUpdate(awareness, bytes, 'remote')
      } else if (topic === 'board-ctl') {
        // A peer just joined and said hello — hand them our full doc + awareness.
        publishBoardData(Y.encodeStateAsUpdate(ydoc), 'board')
        const clients = Array.from(awareness.getStates().keys())
        if (clients.length) publishBoardData(encodeAwarenessUpdate(awareness, clients), 'board-aware')
      }
    })

    // Publish our identity so peers render our cursor with the right name/colour.
    awareness.setLocalStateField('user', boardUser)

    // Announce ourselves so existing peers sync us up. One-byte sentinel.
    publishBoardData(new Uint8Array([1]), 'board-ctl')

    return () => {
      ydoc.off('update', onUpdate)
      unsub?.()
      // Tell peers our cursor is gone (broadcast the removal) BEFORE detaching.
      removeAwarenessStates(awareness, [awareness.clientID], 'unmount')
      awareness.off('update', onAware)
      awareness.destroy()
    }
  }, [ydoc, awareness, publishBoardData, onBoardData, boardUser])

  return (
    <aside className="panel panel-board" aria-label="Whiteboard">
      <header className="panel-head">
        <h2>Whiteboard</h2>
        <button type="button" className="icon-btn" aria-label="Close whiteboard" onClick={onClose}>
          <CloseIcon width={18} height={18} />
        </button>
      </header>
      <div className="board-wrap">
        <BoardApp ydoc={ydoc} awareness={awareness} user={boardUser} boardId={roomId} />
      </div>
    </aside>
  )
}
