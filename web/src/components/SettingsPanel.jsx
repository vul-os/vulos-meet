import { CloseIcon } from './Icons.jsx'

function Select({ label, value, options, onChange }) {
  return (
    <label className="field">
      <span className="field-label">{label}</span>
      <div className="select">
        <select value={value || ''} onChange={(e) => onChange(e.target.value)}>
          {(!options || options.length === 0) && <option value="">System default</option>}
          {options?.map((o) => (
            <option key={o.deviceId} value={o.deviceId}>
              {o.label}
            </option>
          ))}
        </select>
      </div>
    </label>
  )
}

export default function SettingsPanel({ devices, selected, onSelect, theme, onTheme, onClose }) {
  return (
    <aside className="panel" aria-label="Settings">
      <header className="panel-head">
        <h2>Settings</h2>
        <button type="button" className="icon-btn" aria-label="Close settings" onClick={onClose}>
          <CloseIcon width={18} height={18} />
        </button>
      </header>
      <div className="settings-body">
        <Select label="Camera" value={selected.camera} options={devices.cameras} onChange={(id) => onSelect('camera', id)} />
        <Select label="Microphone" value={selected.mic} options={devices.mics} onChange={(id) => onSelect('mic', id)} />
        <Select label="Speaker" value={selected.speaker} options={devices.speakers} onChange={(id) => onSelect('speaker', id)} />
        <div className="field">
          <span className="field-label">Appearance</span>
          <div className="seg">
            <button type="button" className={theme === 'dark' ? 'on' : ''} onClick={() => onTheme('dark')}>
              Dark
            </button>
            <button type="button" className={theme === 'light' ? 'on' : ''} onClick={() => onTheme('light')}>
              Light
            </button>
          </div>
        </div>
      </div>
    </aside>
  )
}
