#!/usr/bin/env node
/**
 * Vulos Meet — Playwright screenshotter (demo / mock mode).
 *
 * The call UI needs a real SFU, a camera, and a minted token to run for real —
 * none of which exist in CI. So this captures the client in its built-in DEMO
 * mode (?demo=<scene>): the React app seeds a fake LiveKit room (static
 * participant tiles, a mock screen-share, connecting/pre-join states) with no
 * network and no media devices. Everything rendered is real chrome over seeded
 * state.
 *
 * It serves the built dist/ over a tiny static file server (SPA fallback) and
 * drives Chromium headless. No Go binary and no livekit-server are required.
 *
 * Usage:
 *   npm run build && npm run screenshots
 * Prereqs:
 *   npx playwright install chromium
 *
 * Output: docs/screenshots/{pre-join,in-room,screen-share,mobile,...}.png
 */

import { chromium } from 'playwright'
import { createServer } from 'node:http'
import { readFile, mkdir, writeFile, stat } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'
import path from 'node:path'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const ROOT = path.resolve(__dirname, '..')
const DIST = path.join(ROOT, 'dist')
const OUT = path.resolve(ROOT, '..', 'docs', 'screenshots')
const PORT = 4178
const BASE = `http://localhost:${PORT}`

const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

const MIME = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.svg': 'image/svg+xml',
  '.png': 'image/png',
  '.json': 'application/json',
  '.woff2': 'font/woff2',
}

// Minimal static server with SPA fallback to index.html (mirrors the Go embed
// handler) so deep links and ?demo= routes resolve.
function serveDist() {
  return createServer(async (req, res) => {
    try {
      const url = new URL(req.url, BASE)
      let rel = decodeURIComponent(url.pathname).replace(/^\/+/, '')
      if (rel === '') rel = 'index.html'
      let file = path.join(DIST, rel)
      try {
        const s = await stat(file)
        if (s.isDirectory()) file = path.join(file, 'index.html')
      } catch {
        file = path.join(DIST, 'index.html') // SPA fallback
      }
      const body = await readFile(file)
      res.writeHead(200, { 'Content-Type': MIME[path.extname(file)] || 'application/octet-stream' })
      res.end(body)
    } catch {
      res.writeHead(404)
      res.end('not found')
    }
  })
}

const SCENES = [
  { name: 'pre-join', url: '/standup-2026-06-27?demo=prejoin&name=Amara%20Ndlovu', vp: { width: 1440, height: 900 }, wait: '.prejoin', settle: 1800, desc: 'Pre-join lobby — preview, device pickers, join' },
  { name: 'in-room', url: '/?demo=in-room', vp: { width: 1440, height: 900 }, wait: '.room', desc: 'In-room responsive video grid + control bar' },
  { name: 'screen-share', url: '/?demo=screen-share', vp: { width: 1440, height: 900 }, wait: '.stage-layout', desc: 'Presenter focus — screen-share with filmstrip' },
  { name: 'chat', url: '/?demo=in-room', vp: { width: 1440, height: 900 }, wait: '.room', desc: 'In-call chat side panel', click: '[aria-label="Chat"]' },
  { name: 'participants', url: '/?demo=in-room', vp: { width: 1440, height: 900 }, wait: '.room', desc: 'Participant list side panel', click: '[aria-label="Participants"]' },
  { name: 'connecting', url: '/?demo=connecting', vp: { width: 1440, height: 900 }, wait: '.status-screen', desc: 'Connecting state' },
  { name: 'mobile', url: '/?demo=in-room', vp: { width: 390, height: 844 }, wait: '.room', desc: 'Mobile call layout (390×844)' },
]

async function main() {
  // dist must be built.
  try {
    await stat(path.join(DIST, 'index.html'))
  } catch {
    console.error('dist/ not built. Run `npm run build` first.')
    process.exit(1)
  }
  await mkdir(OUT, { recursive: true })

  const server = serveDist()
  await new Promise((r) => server.listen(PORT, r))
  console.log(`\nVulos Meet screenshotter`)
  console.log(`  serving : ${DIST}`)
  console.log(`  output  : ${path.relative(process.cwd(), OUT)}/\n`)

  // Fake media devices so the pre-join preview shows a (synthetic) camera feed
  // and device pickers populate, with no real hardware.
  const browser = await chromium.launch({
    headless: true,
    args: ['--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream'],
  })
  const results = []
  try {
    for (const scene of SCENES) {
      const ctx = await browser.newContext({
        viewport: scene.vp,
        colorScheme: 'dark',
        deviceScaleFactor: 2,
        permissions: ['camera', 'microphone'],
      })
      const page = await ctx.newPage()
      page.on('pageerror', (e) => console.log(`  [warn] ${scene.name}: ${e.message}`))
      await page.goto(`${BASE}${scene.url}`, { waitUntil: 'networkidle', timeout: 20_000 })
      await page.waitForSelector(scene.wait, { timeout: 10_000 }).catch(() => {})
      if (scene.click) {
        await page.click(scene.click).catch(() => {})
        await sleep(400)
      }
      await sleep(scene.settle ?? 600)
      const outPath = path.join(OUT, `${scene.name}.png`)
      await page.screenshot({ path: outPath, fullPage: false })
      console.log(`  [ok] ${scene.name}.png — ${scene.desc}`)
      results.push({ name: scene.name, desc: scene.desc })
      await ctx.close()
    }
  } finally {
    await browser.close()
    server.close()
  }

  const md = [
    '# docs/screenshots',
    '',
    'Generated by `npm run screenshots` (web/scripts/screenshots.mjs) against the',
    "client's built-in **demo mode** (`?demo=<scene>`): a seeded fake LiveKit room",
    'rendered with no real SFU, network, camera, or token. Regenerate after a UI',
    'change with `npm --prefix web run build && npm --prefix web run screenshots`.',
    '',
    '| File | Surface |',
    '|------|---------|',
    ...results.map((r) => `| ${r.name}.png | ${r.desc} |`),
    '',
    '## Note',
    '',
    'Live media (real camera/WebRTC tracks) only renders against a running SFU,',
    'so demo tiles show seeded placeholders (hue gradients / a mock screen-share',
    'dashboard) in place of live video. The chrome, layout, and interactions are',
    'the real production components.',
  ].join('\n')
  await writeFile(path.join(OUT, 'README.md'), md + '\n')

  console.log(`\nDone — ${results.length} screenshots in ${path.relative(process.cwd(), OUT)}/`)
}

main().catch((err) => {
  console.error('Fatal:', err)
  process.exit(1)
})
