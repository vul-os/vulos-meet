#!/bin/sh
# docker-entrypoint.sh — render /etc/vulos-meet/config.yaml from env (so secrets
# never ship in the image), then exec vulos-meet. The LiveKit api_key/api_secret
# pair MUST match vulos-cloud MEET-CP-01 (the cloud mints, vulos-meet verifies).
#
# Env (inject via `fly secrets set`):
#   MEET_LIVEKIT_API_KEY      (required) — shared LiveKit key
#   MEET_LIVEKIT_API_SECRET   (required) — shared LiveKit secret
#   MEET_REGION               (required) — Fly region this box advertises
#   MEET_ADMIN_TOKEN          (required) — guards /admin/* (env wins over config)
#   MEET_SIGNAL_ADDR          (default 0.0.0.0:7883) — public gate; must be reachable by the Fly edge
#   MEET_RTC_PORT_START/END   (default 50000/50200) — UDP media range; MUST match fly.toml's UDP service
#   MEET_EGRESS_ENDPOINT      (optional) — cloud egress hook URL
#   MEET_CLUSTER_REDIS_ADDR / MEET_CLUSTER_REDIS_PASSWORD (optional) — cascading SFU
set -eu

CONFIG_PATH="${VULOS_MEET_CONFIG:-/etc/vulos-meet/config.yaml}"
SIGNAL_ADDR="${MEET_SIGNAL_ADDR:-0.0.0.0:7883}"
RTC_START="${MEET_RTC_PORT_START:-50000}"
RTC_END="${MEET_RTC_PORT_END:-50200}"

# Fail fast on the required secrets rather than booting a misconfigured SFU.
: "${MEET_LIVEKIT_API_KEY:?MEET_LIVEKIT_API_KEY is required}"
: "${MEET_LIVEKIT_API_SECRET:?MEET_LIVEKIT_API_SECRET is required}"
: "${MEET_REGION:?MEET_REGION is required}"

{
  echo "region: \"${MEET_REGION}\""
  echo "livekit:"
  echo "  api_key: \"${MEET_LIVEKIT_API_KEY}\""
  echo "  api_secret: \"${MEET_LIVEKIT_API_SECRET}\""
  echo "  signaling_addr: \"127.0.0.1:7880\""   # livekit-server child stays loopback; only the gate is public
  echo "  rtc_port_range_start: ${RTC_START}"
  echo "  rtc_port_range_end: ${RTC_END}"
  echo "signal:"
  echo "  addr: \"${SIGNAL_ADDR}\""              # the only public surface (Fly edge → here)
  echo "admin:"
  echo "  addr: \"127.0.0.1:7881\""              # internal only (reachable via fly ssh / 6PN)
  if [ -n "${MEET_EGRESS_ENDPOINT:-}" ]; then
    echo "recording:"
    echo "  egress_endpoint: \"${MEET_EGRESS_ENDPOINT}\""
  fi
  if [ -n "${MEET_CLUSTER_REDIS_ADDR:-}" ]; then
    # Cascading-SFU node-discovery Redis. PROVISION it with docker-compose.yml
    # (the `redis` service) for self-host, or fly-redis.toml for Fly — both wire
    # MEET_CLUSTER_REDIS_ADDR at this app. The password is read from env by the
    # wrap (applyEnv), never written here. The optional DB index is rendered so
    # the wrap's boot self-check PINGs the SAME keyspace LiveKit will use.
    echo "cluster:"
    echo "  redis:"
    echo "    addr: \"${MEET_CLUSTER_REDIS_ADDR}\""
    if [ -n "${MEET_CLUSTER_REDIS_DB:-}" ]; then
      echo "    db: ${MEET_CLUSTER_REDIS_DB}"
    fi
  fi
} > "${CONFIG_PATH}"

# MEET_ADMIN_TOKEN / MEET_CLUSTER_REDIS_PASSWORD are read from env by vulos-meet
# itself (applyEnv) and intentionally NOT written to disk.
exec vulos-meet --config "${CONFIG_PATH}" "$@"
