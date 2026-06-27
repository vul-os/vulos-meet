import VideoTile from './VideoTile.jsx'

// Layout policy:
//   - A presenter (screen-share) OR a single dominant active speaker drives a
//     focus layout: big stage + a scrollable filmstrip of the rest.
//   - Otherwise a responsive grid that adapts column count to participant count
//     (and collapses to a single column / stacked layout on narrow mobile).
export default function VideoGrid({ tiles, presenter }) {
  if (!tiles || tiles.length === 0) return <div className="grid empty" />

  let focusKey = presenter
  if (!focusKey) {
    const speaker = tiles.find((t) => t.speaking && t.kind === 'camera')
    if (speaker && tiles.length > 2) focusKey = speaker.key
  }

  if (focusKey) {
    const stage = tiles.find((t) => t.key === focusKey) || tiles[0]
    const rest = tiles.filter((t) => t.key !== stage.key)
    return (
      <div className="stage-layout">
        <div className="stage">
          <VideoTile tile={stage} focus />
        </div>
        {rest.length > 0 && (
          <div className="filmstrip" role="list" aria-label="Other participants">
            {rest.map((t) => (
              <div role="listitem" key={t.key} className="filmstrip-item">
                <VideoTile tile={t} />
              </div>
            ))}
          </div>
        )}
      </div>
    )
  }

  const n = tiles.length
  const cols = n <= 1 ? 1 : n <= 4 ? 2 : n <= 9 ? 3 : 4
  return (
    <div className={`grid cols-${cols}`} data-count={n} role="list" aria-label="Participants">
      {tiles.map((t) => (
        <div role="listitem" key={t.key} className="grid-cell">
          <VideoTile tile={t} />
        </div>
      ))}
    </div>
  )
}
