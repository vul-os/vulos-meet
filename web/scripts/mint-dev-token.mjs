#!/usr/bin/env node
/**
 * mint-dev-token — a LOCAL-DEV helper that mints a VULOS-MEET/1 access token.
 *
 * vulos-meet itself NEVER mints tokens (spec/TOKEN.md): in production they are
 * minted by vulos-cloud's control plane and handed to the client via a meeting
 * link. This script exists ONLY so a developer running a standalone box can get
 * a token to open the client. It signs an HS256 LiveKit-compatible JWT with the
 * same (api_key, api_secret) the box verifies with.
 *
 * Usage:
 *   node scripts/mint-dev-token.mjs \
 *     --key APIxxxx --secret <secret> --room standup --tenant acme \
 *     --identity u_dev --name "Dev User"
 *
 * Or via env: MEET_LIVEKIT_API_KEY / MEET_LIVEKIT_API_SECRET.
 *
 * The token binds `name`=tenant and `video.room`=<tenant><sep><room> so the
 * tenant-binding invariant holds. Prints a ready-to-open URL.
 */

import { createHmac } from 'node:crypto'

function arg(name, fallback) {
  const i = process.argv.indexOf(`--${name}`)
  return i >= 0 && process.argv[i + 1] ? process.argv[i + 1] : fallback
}

const apiKey = arg('key', process.env.MEET_LIVEKIT_API_KEY)
const apiSecret = arg('secret', process.env.MEET_LIVEKIT_API_SECRET)
const tenant = arg('tenant', 'acme')
const room = arg('room', 'demo')
const sep = arg('sep', ':')
const identity = arg('identity', 'u_dev')
const name = arg('name', tenant) // top-level grant = tenant audience
const ttlH = parseFloat(arg('ttl', '6'))
const base = arg('base', 'http://localhost:7883')

if (!apiKey || !apiSecret) {
  console.error('error: --key and --secret (or MEET_LIVEKIT_API_KEY / MEET_LIVEKIT_API_SECRET) are required')
  process.exit(2)
}

const b64url = (buf) => Buffer.from(buf).toString('base64url')

function sign(payload) {
  const header = b64url(JSON.stringify({ alg: 'HS256', typ: 'JWT' }))
  const body = b64url(JSON.stringify(payload))
  const data = `${header}.${body}`
  const sig = createHmac('sha256', apiSecret).update(data).digest('base64url')
  return `${data}.${sig}`
}

const now = Math.floor(Date.now() / 1000)
const fullRoom = `${tenant}${sep}${room}`

const token = sign({
  iss: apiKey, // LiveKit requires iss == api_key
  sub: identity,
  nbf: now - 5,
  exp: now + Math.round(ttlH * 3600),
  name: tenant, // VULOS-MEET/1 tenant audience (binds to room prefix)
  video: {
    room: fullRoom,
    roomJoin: true,
    canPublish: true,
    canSubscribe: true,
    canPublishData: true,
  },
  metadata: name,
})

const url = `${base}/${encodeURIComponent(fullRoom)}?token=${token}`

console.log('\nVULOS-MEET/1 dev token (HS256)\n')
console.log(token)
console.log('\nOpen the client at:\n')
console.log(url + '\n')
