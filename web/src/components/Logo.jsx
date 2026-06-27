// Vulos mark — the stylised "V" with a teardrop counter, mono wordmark.
// Restrained, OSS-native (dark slate glyph, a single brand-purple accent dot).

export function LogoMark({ size = 28 }) {
  return (
    <svg width={size} height={size} viewBox="0 0 64 64" role="img" aria-label="Vulos">
      <path d="M14 16 L32 48 L50 16 L41 16 L32 34 L23 16 Z" fill="currentColor" />
      <circle cx="32" cy="22" r="3.6" fill="var(--brand)" />
    </svg>
  )
}

export function Logo({ size = 24, label = 'Meet' }) {
  return (
    <span className="logo" aria-label={`Vulos ${label}`}>
      <span className="logo-mark" style={{ color: 'var(--text-primary)' }}>
        <LogoMark size={size} />
      </span>
      <span className="logo-word">
        Vulos<span className="logo-sub"> {label}</span>
      </span>
    </span>
  )
}
