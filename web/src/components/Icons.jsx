// Inline, currentColor SVG icons. Stroke-based, 24px grid, tuned to read at the
// small sizes used in the control bar. aria-hidden — labels live on the buttons.

const base = {
  width: 22,
  height: 22,
  viewBox: '0 0 24 24',
  fill: 'none',
  stroke: 'currentColor',
  strokeWidth: 1.7,
  strokeLinecap: 'round',
  strokeLinejoin: 'round',
  'aria-hidden': true,
}

export const MicIcon = (p) => (
  <svg {...base} {...p}>
    <rect x="9" y="3" width="6" height="11" rx="3" />
    <path d="M5 11a7 7 0 0 0 14 0" />
    <path d="M12 18v3" />
  </svg>
)

export const MicOffIcon = (p) => (
  <svg {...base} {...p}>
    <path d="M15 9.3V6a3 3 0 0 0-5.9-.7" />
    <path d="M9 9v2a3 3 0 0 0 4.5 2.6" />
    <path d="M5 11a7 7 0 0 0 10.8 5.9M19 11a7 7 0 0 1-.6 2.8" />
    <path d="M12 18v3" />
    <path d="M3 3l18 18" />
  </svg>
)

export const CamIcon = (p) => (
  <svg {...base} {...p}>
    <rect x="2" y="6" width="13" height="12" rx="2.5" />
    <path d="M15 10.5 22 7v10l-7-3.5" />
  </svg>
)

export const CamOffIcon = (p) => (
  <svg {...base} {...p}>
    <path d="M15 10.5 22 7v10l-5-2.5" />
    <path d="M11 6h2a2.5 2.5 0 0 1 2 1" />
    <path d="M2 8.5V16a2 2 0 0 0 2 2h9" />
    <path d="M3 3l18 18" />
  </svg>
)

export const ScreenIcon = (p) => (
  <svg {...base} {...p}>
    <rect x="3" y="4" width="18" height="12" rx="2" />
    <path d="M8 20h8M12 16v4" />
    <path d="M12 12V8M12 8l-2.2 2.2M12 8l2.2 2.2" />
  </svg>
)

export const HandIcon = (p) => (
  <svg {...base} {...p}>
    <path d="M9 11V5.5a1.5 1.5 0 0 1 3 0V11" />
    <path d="M12 11V4.5a1.5 1.5 0 0 1 3 0V11" />
    <path d="M15 11V6.5a1.5 1.5 0 0 1 3 0V14a6 6 0 0 1-6 6h-1a6 6 0 0 1-5.2-3l-2.3-4a1.5 1.5 0 0 1 2.5-1.6L9 13.5V8a1.5 1.5 0 0 1 3 0" />
  </svg>
)

export const LeaveIcon = (p) => (
  <svg {...base} {...p}>
    <path d="M21 15.5c-2.5.8-5.3 1.2-9 1.2s-6.5-.4-9-1.2v-2.6c0-.7.4-1.3 1-1.5C6 10.4 9 10 12 10s6 .4 8 .9c.6.2 1 .8 1 1.5z" />
    <path d="M7.5 11.2V9.2M16.5 11.2V9.2" />
  </svg>
)

export const ChatIcon = (p) => (
  <svg {...base} {...p}>
    <path d="M4 5h16a1 1 0 0 1 1 1v9a1 1 0 0 1-1 1H9l-4 3v-3H4a1 1 0 0 1-1-1V6a1 1 0 0 1 1-1z" />
  </svg>
)

export const PeopleIcon = (p) => (
  <svg {...base} {...p}>
    <circle cx="9" cy="8" r="3" />
    <path d="M3 19a6 6 0 0 1 12 0" />
    <path d="M16 5.5a3 3 0 0 1 0 5.5M21 19a6 6 0 0 0-4-5.7" />
  </svg>
)

export const SettingsIcon = (p) => (
  <svg {...base} {...p}>
    <circle cx="12" cy="12" r="3" />
    <path d="M19.4 12a7.4 7.4 0 0 0-.1-1.2l2-1.5-2-3.4-2.3 1a7 7 0 0 0-2-1.2l-.3-2.5h-4l-.3 2.5a7 7 0 0 0-2 1.2l-2.3-1-2 3.4 2 1.5a7.4 7.4 0 0 0 0 2.4l-2 1.5 2 3.4 2.3-1a7 7 0 0 0 2 1.2l.3 2.5h4l.3-2.5a7 7 0 0 0 2-1.2l2.3 1 2-3.4-2-1.5c.1-.4.1-.8.1-1.2z" />
  </svg>
)

export const BoardIcon = (p) => (
  <svg {...base} {...p}>
    <rect x="3" y="4" width="18" height="13" rx="2" />
    <path d="M9 20l3-3 3 3" />
    <path d="M7 12.5l3-3 2.5 2.5L17 7.5" />
  </svg>
)

export const CloseIcon = (p) => (
  <svg {...base} {...p}>
    <path d="M6 6l12 12M18 6 6 18" />
  </svg>
)

export const SendIcon = (p) => (
  <svg {...base} {...p}>
    <path d="M4 12 20 4l-6 16-3.5-6.5L4 12z" />
  </svg>
)

export const LinkIcon = (p) => (
  <svg {...base} {...p}>
    <path d="M10 14a4 4 0 0 0 5.7 0l3-3a4 4 0 0 0-5.7-5.7L11.5 6.5" />
    <path d="M14 10a4 4 0 0 0-5.7 0l-3 3a4 4 0 0 0 5.7 5.7L12.5 17" />
  </svg>
)

export const CheckIcon = (p) => (
  <svg {...base} {...p}>
    <path d="M5 12.5 10 17 19 7" />
  </svg>
)

export const ChevronDownIcon = (p) => (
  <svg {...base} {...p}>
    <path d="M6 9l6 6 6-6" />
  </svg>
)

export const Spinner = (p) => (
  <svg {...base} {...p} className={`spin ${p?.className || ''}`}>
    <path d="M12 3a9 9 0 1 0 9 9" />
  </svg>
)
