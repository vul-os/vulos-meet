import { useEffect, useMemo } from 'react'
import { BoardApp, createBoardDoc } from '@vulos/board-ui'
import * as Y from 'yjs'
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
// a tiny Yjs provider over it:
//   - outbound: local doc updates -> publishBoardData(update, 'board')
//   - inbound:  'board' bytes -> Y.applyUpdate(doc, bytes, 'remote')
// The 'remote' origin tag stops applied updates from being re-broadcast.
//
// Late join: on mount we announce ourselves on the 'board-ctl' topic; any peer
// that already holds doc state replies with the full state (encodeStateAsUpdate)
// on the 'board' topic. Applying full state is idempotent in Yjs, so duplicate
// replies are harmless.
export default function WhiteboardPanel({ user, roomId, actions, onClose }) {
  const ydoc = useMemo(() => createBoardDoc(), [])
  const { publishBoardData, onBoardData } = actions

  useEffect(() => {
    if (!publishBoardData || !onBoardData) return

    const onUpdate = (update, origin) => {
      if (origin !== 'remote') publishBoardData(update, 'board')
    }
    ydoc.on('update', onUpdate)

    const unsub = onBoardData((topic, bytes) => {
      if (topic === 'board') {
        Y.applyUpdate(ydoc, bytes, 'remote')
      } else if (topic === 'board-ctl') {
        // A peer just joined and said hello — hand them our full state.
        publishBoardData(Y.encodeStateAsUpdate(ydoc), 'board')
      }
    })

    // Announce ourselves so existing peers sync us up. One-byte sentinel.
    publishBoardData(new Uint8Array([1]), 'board-ctl')

    return () => {
      ydoc.off('update', onUpdate)
      unsub?.()
    }
  }, [ydoc, publishBoardData, onBoardData])

  const boardUser = useMemo(
    () => ({ id: user?.id || 'guest', name: user?.name || 'You', color: colorFor(user?.id) }),
    [user?.id, user?.name],
  )

  return (
    <aside className="panel panel-board" aria-label="Whiteboard">
      <header className="panel-head">
        <h2>Whiteboard</h2>
        <button type="button" className="icon-btn" aria-label="Close whiteboard" onClick={onClose}>
          <CloseIcon width={18} height={18} />
        </button>
      </header>
      <div className="board-wrap">
        <BoardApp ydoc={ydoc} user={boardUser} boardId={roomId} />
      </div>
    </aside>
  )
}
