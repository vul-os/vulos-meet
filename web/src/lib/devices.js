// Media-device helpers for the pre-join lobby. Kept dependency-free (raw
// mediaDevices) so the preview works before any LiveKit Room is created.

export async function enumerate() {
  if (!navigator.mediaDevices?.enumerateDevices) return { cameras: [], mics: [], speakers: [] }
  const all = await navigator.mediaDevices.enumerateDevices()
  const map = (kind) =>
    all
      .filter((d) => d.kind === kind)
      .map((d, i) => ({ deviceId: d.deviceId, label: d.label || fallbackLabel(kind, i) }))
  return {
    cameras: map('videoinput'),
    mics: map('audioinput'),
    speakers: map('audiooutput'),
  }
}

function fallbackLabel(kind, i) {
  const base = { videoinput: 'Camera', audioinput: 'Microphone', audiooutput: 'Speaker' }[kind] || 'Device'
  return `${base} ${i + 1}`
}

/**
 * Acquire a preview stream for the lobby. Resolves to a MediaStream, or throws
 * a typed error: 'permission-denied' | 'not-found' | 'error'.
 */
export async function getPreviewStream({ cameraId, micId, video = true, audio = true } = {}) {
  if (!navigator.mediaDevices?.getUserMedia) {
    const e = new Error('mediaDevices unavailable')
    e.code = 'error'
    throw e
  }
  const constraints = {
    video: video ? (cameraId ? { deviceId: { exact: cameraId } } : true) : false,
    audio: audio ? (micId ? { deviceId: { exact: micId } } : true) : false,
  }
  try {
    return await navigator.mediaDevices.getUserMedia(constraints)
  } catch (err) {
    const e = new Error(err?.message || 'getUserMedia failed')
    if (err?.name === 'NotAllowedError' || err?.name === 'SecurityError') e.code = 'permission-denied'
    else if (err?.name === 'NotFoundError' || err?.name === 'OverconstrainedError') e.code = 'not-found'
    else e.code = 'error'
    throw e
  }
}

export function stopStream(stream) {
  if (!stream) return
  for (const t of stream.getTracks()) {
    try {
      t.stop()
    } catch {
      /* already stopped */
    }
  }
}
