// Connection-quality presentation helpers, shared by the tile badge and the
// participants panel. Kept pure (no React / no LiveKit import) so it is trivially
// unit-tested and both surfaces agree on bars/label/tone for a given quality.
//
// `quality` is one of the normalised labels LiveRoom emits:
//   'excellent' | 'good' | 'poor' | 'lost' | 'unknown'

const TABLE = {
  excellent: { bars: 4, tone: 'ok', label: 'Excellent connection' },
  good: { bars: 3, tone: 'ok', label: 'Good connection' },
  poor: { bars: 1, tone: 'warn', label: 'Poor connection' },
  lost: { bars: 0, tone: 'danger', label: 'Connection lost' },
  unknown: { bars: 0, tone: 'muted', label: 'Checking connection' },
}

export function qualityInfo(quality) {
  return TABLE[quality] || TABLE.unknown
}

// Only surface a quality badge when it is actionable — a perfect connection
// needs no chrome. We show it for poor/lost (and keep it hidden otherwise).
export function shouldShowQuality(quality) {
  return quality === 'poor' || quality === 'lost'
}
