#!/usr/bin/env bash
# Capture Apple KVS credentials (Cookie + DSID) from live Apple Podcasts traffic via Proxyman.
#
# Apple Podcasts must start while the proxy is disabled; the script handles this automatically
# when Podcasts is not already running.
#
# Usage:
#   ./scripts/capture-kvs-creds.sh              # prints export statements to stdout
#   ./scripts/capture-kvs-creds.sh --write      # writes to .creds in the repo root
#   ./scripts/capture-kvs-creds.sh --write /path/to/credfile

set -euo pipefail

PROXYMAN_CLI="/Applications/Proxyman.app/Contents/MacOS/proxyman-cli"
DOMAIN="bookkeeper.itunes.apple.com"
HAR_TMP="$(mktemp /tmp/kvs-capture-XXXXXX.har)"

# Track whether we disabled the proxy so we always restore it on exit.
PROXY_DISABLED_BY_US=false

cleanup() {
  rm -f "$HAR_TMP"
  if [ "$PROXY_DISABLED_BY_US" = true ]; then
    echo "→ Restoring Proxyman proxy..." >&2
    "$PROXYMAN_CLI" proxy on >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

# ── Flags ────────────────────────────────────────────────────────────────────

WRITE_MODE=false
CREDS_FILE="$(cd "$(dirname "$0")/.." && pwd)/.creds"

case "${1:-}" in
  --write)
    WRITE_MODE=true
    if [ -n "${2:-}" ]; then CREDS_FILE="$2"; fi
    ;;
  --help|-h)
    echo "Usage: $0 [--write [PATH]]"
    echo ""
    echo "Captures KVS credentials from Apple Podcasts traffic via Proxyman."
    echo "  (no flag)  — prints 'export APPLE_KVS_*=...' to stdout"
    echo "  --write    — writes credentials to .creds (repo root) or PATH"
    echo ""
    echo "Apple Podcasts must be started while the Proxyman proxy is disabled."
    echo "If Podcasts is not already running, this script disables the proxy,"
    echo "launches Podcasts, waits for it to start, then re-enables the proxy"
    echo "before waiting for a KVS sync."
    exit 0
    ;;
esac

# ── Prerequisites ─────────────────────────────────────────────────────────────

if [ ! -x "$PROXYMAN_CLI" ]; then
  echo "error: Proxyman is not installed at /Applications/Proxyman.app" >&2
  echo "       Install it from https://proxyman.io" >&2
  exit 1
fi

if ! command -v python3 &>/dev/null && ! command -v jq &>/dev/null; then
  echo "error: requires python3 or jq to parse HAR output" >&2
  exit 1
fi

# ── Ensure Proxyman is running ────────────────────────────────────────────────

if ! pgrep -x "Proxyman" &>/dev/null; then
  echo "→ Opening Proxyman..." >&2
  open -a Proxyman
  sleep 3
fi

# ── Check for existing traffic first ─────────────────────────────────────────

echo "→ Checking for $DOMAIN traffic in Proxyman session..." >&2

"$PROXYMAN_CLI" export-log \
  --mode domains \
  --domains "$DOMAIN" \
  --format har \
  --output "$HAR_TMP" >/dev/null 2>&1

entry_count() {
  if command -v python3 &>/dev/null; then
    python3 -c "import json; d=json.load(open('$HAR_TMP')); print(len(d['log']['entries']))" 2>/dev/null || echo 0
  else
    jq '.log.entries | length' "$HAR_TMP" 2>/dev/null || echo 0
  fi
}

ENTRY_COUNT=$(entry_count)

# ── Trigger a sync if no traffic captured ─────────────────────────────────────

if [ "$ENTRY_COUNT" -eq 0 ]; then
  if pgrep -x "Podcasts" &>/dev/null; then
    # Podcasts is already running — bring it to the foreground to trigger a sync.
    echo "→ Bringing Apple Podcasts to the foreground to trigger a KVS sync..." >&2
    open -a "Podcasts"
    echo "→ Waiting 10 seconds for sync..." >&2
    sleep 10
  else
    # Podcasts is not running. It must start while the proxy is disabled, otherwise
    # it cannot connect during launch and will not perform a KVS sync afterward.
    echo "→ Apple Podcasts is not running." >&2
    echo "→ Disabling Proxyman proxy so Podcasts can start cleanly..." >&2
    "$PROXYMAN_CLI" proxy off >/dev/null 2>&1
    PROXY_DISABLED_BY_US=true

    echo "→ Launching Apple Podcasts..." >&2
    open -a "Podcasts"

    echo "→ Waiting 8 seconds for Podcasts to start..." >&2
    sleep 8

    echo "→ Re-enabling Proxyman proxy..." >&2
    "$PROXYMAN_CLI" proxy on >/dev/null 2>&1
    PROXY_DISABLED_BY_US=false

    echo "→ Waiting 15 seconds for KVS sync..." >&2
    sleep 15
  fi

  "$PROXYMAN_CLI" export-log \
    --mode domains \
    --domains "$DOMAIN" \
    --format har \
    --output "$HAR_TMP" >/dev/null 2>&1

  ENTRY_COUNT=$(entry_count)

  if [ "$ENTRY_COUNT" -eq 0 ]; then
    echo "" >&2
    echo "error: No $DOMAIN requests captured." >&2
    echo "" >&2
    echo "Make sure:" >&2
    echo "  1. Proxyman has '$DOMAIN' in its SSL Proxying list" >&2
    echo "  2. The Proxyman root certificate is trusted in System Settings" >&2
    echo "  3. Try running the script again — Podcasts may need another sync cycle" >&2
    exit 1
  fi
fi

echo "→ Found $ENTRY_COUNT request(s) — extracting credentials..." >&2

# ── Parse Cookie and DSID from the HAR ───────────────────────────────────────

if command -v python3 &>/dev/null; then
  read -r DSID COOKIE_HDR < <(python3 - "$HAR_TMP" <<'PYEOF'
import json, sys

with open(sys.argv[1]) as f:
    har = json.load(f)

# Use the most recent entry
entry = har["log"]["entries"][-1]["request"]

dsid = ""
cookie = ""

for h in entry.get("headers", []):
    name = h["name"].lower()
    if name == "cookie":
        cookie = h["value"]
    elif name in ("x-dsid", "icloud-dsid") and not dsid:
        dsid = h["value"]

# Fallback: derive DSID from the X-Dsid cookie
if not dsid:
    for c in entry.get("cookies", []):
        if c["name"] == "X-Dsid":
            dsid = c["value"]
            break

print(dsid, cookie)
PYEOF
)
else
  DSID=$(jq -r '[.log.entries[-1].request.headers[] | select(.name | ascii_downcase == "x-dsid")] | .[0].value // ""' "$HAR_TMP")
  COOKIE_HDR=$(jq -r '[.log.entries[-1].request.headers[] | select(.name | ascii_downcase == "cookie")] | .[0].value // ""' "$HAR_TMP")
fi

if [ -z "$DSID" ] || [ -z "$COOKIE_HDR" ]; then
  echo "error: could not extract DSID or Cookie from the captured request." >&2
  echo "       SSL decryption may not be active for $DOMAIN." >&2
  echo "       Ensure $DOMAIN is in Proxyman's SSL Proxying list." >&2
  exit 1
fi

# ── Output ────────────────────────────────────────────────────────────────────

if [ "$WRITE_MODE" = true ]; then
  {
    printf 'APPLE_KVS_DSID=%s\n' "$DSID"
    printf "APPLE_KVS_COOKIES='%s'\n" "$COOKIE_HDR"
  } > "$CREDS_FILE"
  echo "✓ Credentials written to $CREDS_FILE" >&2
  echo "  Source them with: source $CREDS_FILE" >&2
  echo "  Or: set -a && source $CREDS_FILE && set +a" >&2
else
  echo ""
  echo "export APPLE_KVS_DSID=\"$DSID\""
  echo "export APPLE_KVS_COOKIES=\"$COOKIE_HDR\""
  echo ""
  echo "# Paste the above into your shell, or run:" >&2
  echo "#   eval \$($0)" >&2
fi
