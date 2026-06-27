import { useState } from 'react'
import AppsAndBots from '@vulos/apps-ui'
import '@vulos/apps-ui/styles.css'

// AppsAndBotsView is Vulos Meet's "apps & bots place": a lightweight management
// surface over the shared @vulos/apps platform mounted at /api/apps. It runs in
// product mode (product="meet"), so it lists/installs apps that target Meet and
// reads the SAME GET /api/apps consolidation contract that Workspace aggregates.
//
// The management API is guarded by Meet's admin bearer (MEET_ADMIN_TOKEN), so
// this view collects that token (persisted to localStorage for convenience) and
// hands it to the platform client. Without it the list endpoint returns 401.
const TOKEN_KEY = 'vulos-meet-admin-token'

export default function AppsAndBotsView({ theme = 'dark', onTheme }) {
  const [token, setToken] = useState(() => localStorage.getItem(TOKEN_KEY) || '')
  const [draft, setDraft] = useState(token)

  const save = (e) => {
    e.preventDefault()
    const t = draft.trim()
    setToken(t)
    if (t) localStorage.setItem(TOKEN_KEY, t)
    else localStorage.removeItem(TOKEN_KEY)
  }

  return (
    <div className="apps-view" data-theme={theme}>
      <header className="apps-view__bar">
        <a className="apps-view__home" href="/" title="Back to meetings">← Meet</a>
        <h1 className="apps-view__title">Apps &amp; Bots</h1>
        {onTheme && (
          <button
            type="button"
            className="apps-view__theme"
            onClick={() => onTheme(theme === 'dark' ? 'light' : 'dark')}
          >
            {theme === 'dark' ? 'Light' : 'Dark'}
          </button>
        )}
      </header>

      <form className="apps-view__auth" onSubmit={save}>
        <label htmlFor="apps-admin-token">Admin token</label>
        <input
          id="apps-admin-token"
          type="password"
          autoComplete="off"
          placeholder="MEET_ADMIN_TOKEN"
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
        />
        <button type="submit">Use token</button>
      </form>

      {token ? (
        <AppsAndBots
          mode="product"
          product="meet"
          basePath="/api/apps"
          token={token}
          theme={theme}
          title="Meet Apps & Bots"
          subtitle="Apps and bots that can broadcast into rooms and read room rosters."
        />
      ) : (
        <p className="apps-view__hint">
          Enter the Meet admin token to manage apps &amp; bots for this server.
        </p>
      )}
    </div>
  )
}
