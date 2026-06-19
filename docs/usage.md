---
layout: default
title: Usage
nav_order: 2
---

# Usage

## Installation

**Prerequisites:** Go 1.26+

```sh
git clone https://github.com/tylertufano/podcast-migrate
cd podcast-migrate
go build -o podcast-migrate .
```

Or install directly:

```sh
go install github.com/tylertufano/podcast-migrate@latest
```

### macOS permissions

Reading the Apple Podcasts database requires **Full Disk Access** for your terminal app on macOS Ventura and later. Grant it in **System Settings › Privacy & Security › Full Disk Access**.

If you'd rather not grant Full Disk Access, export your subscriptions manually via **Apple Podcasts › File › Export Subscriptions** and pass the file with `--opml-fallback`. This path carries subscriptions only — play state requires the SQLite database.

---

## Apple Podcasts → Overcast

### Subscriptions only

```sh
# Preview (no files written)
podcast-migrate migrate --from podcasts --to overcast \
  --overcast-out ~/Desktop/overcast-import.opml --dry-run

# Generate the import file
podcast-migrate migrate --from podcasts --to overcast \
  --overcast-out ~/Desktop/overcast-import.opml
```

Then in Overcast: **Settings › Import OPML** and select the generated file.

### With play state

```sh
export OVERCAST_EMAIL="you@example.com"
export OVERCAST_PASSWORD="yourpassword"

# Play state only (auto-subscribes any missing podcasts)
podcast-migrate migrate --from podcasts --to overcast --play-state

# Play state + subscription import file at the same time
podcast-migrate migrate --from podcasts --to overcast \
  --overcast-out ~/Desktop/overcast-import.opml --play-state
```

Any podcast in your Apple library that isn't yet subscribed in Overcast is automatically subscribed before its episodes are updated — Overcast silently drops play-state updates for unsubscribed podcasts. Add `--subscribed-only` to skip this and only update episodes for feeds already in your account.

### Delta sync

```sh
# Only sync episodes whose play state changed in the last 48 hours
podcast-migrate migrate --from podcasts --to overcast \
  --play-state --since 48h
```

`--since` accepts a duration (`48h`, `7d`), a date (`2026-06-01`), or an RFC3339 timestamp.

### Subscriber / private feeds

If you have Apple Podcasts Subscriptions (PSUB) or subscriber-feed episodes, subscribe to the equivalent private feed in Overcast first. The tool automatically detects that the destination has a podcast with a matching title and routes those episodes there — no extra flags needed. Use `--feed-map` to override the auto-match explicitly when titles differ between platforms.

> **Note:** Uses an undocumented Overcast endpoint. It works as of the current release but may break without notice. Always use `--dry-run` to preview before a live run.

---

## Overcast → Apple Podcasts

This direction writes subscriptions and play state via paths that together cover your entire library, syncing to **all your Apple devices** (iPhone, iPad, Mac, podcasts.apple.com) automatically:

- **Subscriptions** (all feeds, including private) → Apple KVS (`bookkeeper.itunes.apple.com`, `com.apple.podcasts` domain)
- **Catalog episodes** (public RSS feeds) → Apple Podcasts web API (`amp-api.podcasts.apple.com`)
- **Private/subscriber-feed episodes** (NPR+, Slate+, etc.) → Apple KVS (`bookkeeper.itunes.apple.com`, `com.apple.upp` domain)

Subscription writes require KVS credentials. Private-feed play state also requires KVS credentials. If KVS credentials are not set, subscriptions and private-feed play state are skipped — only catalog episode play state is written.

### Step 1 — Get your Apple tokens (one-time)

1. Open [podcasts.apple.com](https://podcasts.apple.com) in your browser and sign in
2. Open DevTools (⌥⌘I in Safari, F12 in Chrome) → **Network** tab
3. Mark any episode as played in the web UI
4. Find the `PUT` request to `amp-api.podcasts.apple.com/v1/me/playback/positions/...`
5. Copy two header values from that request:
   - `Authorization` — everything after `Bearer ` (a long JWT string)
   - `media-user-token` — the full value of this header

The Bearer token expires in ~90 days; re-capture it the same way if you get `401` errors. The `media-user-token` lasts longer but also expires eventually.

### Step 2 — Run the migration

With Overcast credentials set, the tool fetches and caches your extended OPML automatically — no manual export needed:

```sh
export OVERCAST_EMAIL="you@example.com"
export OVERCAST_PASSWORD="yourpassword"
export APPLE_BEARER_TOKEN="eyJhbGci..."
export APPLE_MEDIA_USER_TOKEN="0.Apgf..."

# Dry-run first
podcast-migrate migrate --from overcast --to podcasts \
  --play-state --dry-run

# Live run
podcast-migrate migrate --from overcast --to podcasts \
  --play-state
```

The source OPML is cached for 24 hours under `~/Library/Caches/podcast-migrate/overcast-source.opml`. Use `--clear-source-opml-cache` to force a fresh download, or `--overcast-save-source-opml` to save a copy (defaults to `~/Downloads/overcast.opml` when given without a value).

If you prefer to supply an OPML file you downloaded manually, pass it with `--overcast-source-opml`:

```sh
podcast-migrate migrate --from overcast --to podcasts \
  --overcast-source-opml ~/Downloads/overcast.opml --play-state
```

### Private and subscriber-feed episodes (KVS)

Episodes from private feeds (NPR Plus, Slate Plus via supportingcast.fm, independent subscriber feeds, etc.) are not indexed in the Apple catalog and cannot use the web API. They sync via Apple's internal key-value store (KVS), which requires separate credentials — a session cookie captured from a live Apple Podcasts request.

**Step A — One-time Proxyman setup**

1. Install [Proxyman](https://proxyman.io) and enable its system root certificate
2. In Proxyman, add `bookkeeper.itunes.apple.com` to the SSL proxying list (Proxyman → SSL Proxying → Include)

This only needs to be done once. After that, Proxyman silently captures Apple Podcasts KVS traffic whenever it runs in the background.

**Step B — Capture credentials automatically**

With Proxyman running, use the capture script:

```sh
# Print export statements to paste into your shell
./scripts/capture-kvs-creds.sh

# Or evaluate directly
eval $(./scripts/capture-kvs-creds.sh)

# Or write to .creds file (sourced automatically below)
./scripts/capture-kvs-creds.sh --write
```

The script checks Proxyman's current session for `bookkeeper.itunes.apple.com` traffic and extracts credentials from it if present. If no traffic exists, the behavior depends on whether Podcasts is already running:

- **Podcasts is running** — brings it to the foreground to trigger a background sync, then waits.
- **Podcasts is not running** — disables the Proxyman proxy, launches Podcasts, waits for it to initialize, re-enables the proxy, then waits for the KVS sync. This sequence is required because Apple Podcasts must connect to Apple's servers during startup; if the proxy is active at launch it cannot establish those connections and will not perform a KVS sync afterward.

The proxy is always restored on exit, even if the script fails. The captured `APPLE_KVS_DSID` and `APPLE_KVS_COOKIES` values are ready to use immediately.

**Step C — Run the migration**

With KVS credentials set alongside the standard Apple tokens, all three paths run automatically in the same command:

```sh
export OVERCAST_EMAIL="you@example.com"
export OVERCAST_PASSWORD="yourpassword"
export APPLE_BEARER_TOKEN="eyJhbGci..."
export APPLE_MEDIA_USER_TOKEN="0.Apgf..."
export APPLE_KVS_DSID="12345678"
export APPLE_KVS_COOKIES="X-Dsid=12345678; mt-tkn-12345678=ABC...; ..."

podcast-migrate migrate --from overcast --to podcasts
```

The tool logs each path:

```
apple: KVS sync enabled (DSID 12345678) — private-feed episodes will sync via bookkeeper.itunes.apple.com
  kvs: subscribed to "My Private Feed"
  kvs: synced "NPR Politics Podcast+" — "Episode Title [V]"
  ...
marked 9 episode(s) as played via Apple Podcasts web API
```

To migrate subscriptions only (no play state):

```sh
podcast-migrate migrate --from overcast --to podcasts --only-subscriptions
```

**KVS credential expiry:** the captured session cookie is valid for days to weeks. If you see `status=1198` or `status=-4` errors, re-capture credentials using the steps above.

### Limit to specific podcasts

`--podcast` works with any migration direction — pass a case-insensitive substring of the podcast title:

```sh
# Single podcast
podcast-migrate migrate --from overcast --to podcasts \
  --play-state --podcast "fresh air"

# Multiple podcasts
podcast-migrate migrate --from overcast --to podcasts \
  --play-state --podcast "fresh air" --podcast "planet money"

# From a file (one title word or phrase per line)
podcast-migrate migrate --from overcast --to podcasts \
  --play-state --podcast-list ~/my-podcasts.txt
```

---

## Overcast → Overcast (restore play state from an old export)

Useful after cleaning up duplicate public/paid feeds — restore play state from an earlier export:

```sh
export OVERCAST_EMAIL="you@example.com"
export OVERCAST_PASSWORD="yourpassword"

# Dry-run first
podcast-migrate migrate --from overcast --to overcast \
  --overcast-source-opml ~/Downloads/old-export.opml \
  --play-state --force-update --dry-run

# Live run
podcast-migrate migrate --from overcast --to overcast \
  --overcast-source-opml ~/Downloads/old-export.opml \
  --play-state --force-update
```

The source OPML provides your old play history; the tool auto-fetches your current live library as the destination. `--force-update` overwrites episodes the destination already marks as played, which is what you want when restoring from an older snapshot.

If you had both a public feed ("Fresh Air") and a paid feed ("Fresh Air Plus") and your cleaned-up account keeps only one, the tool matches episodes across those feeds by normalizing the title — play state is restored to whichever variant is currently subscribed.

To match against a specific snapshot of the cleaned account instead of the live state:

```sh
podcast-migrate migrate --from overcast --to overcast \
  --overcast-source-opml ~/Downloads/old-export.opml \
  --overcast-match-opml ~/Downloads/cleaned-export.opml \
  --play-state --force-update --dry-run
```

---

## Apple Podcasts → Pocket Casts

```sh
export POCKETCASTS_EMAIL="you@example.com"
export POCKETCASTS_PASSWORD="yourpassword"

# Dry-run first
podcast-migrate migrate --from podcasts --to pocketcasts \
  --play-state --dry-run

# Live run
podcast-migrate migrate --from podcasts --to pocketcasts \
  --play-state
```

Any podcast in your Apple library not yet subscribed in Pocket Casts is automatically subscribed first. Changes sync to all your Pocket Casts devices. Add `--subscribed-only` to only update already-subscribed feeds.

Delta sync:

```sh
podcast-migrate migrate --from podcasts --to pocketcasts \
  --play-state --since 7d
```

> **Note:** Uses an undocumented Pocket Casts endpoint. It works as of the current release but may break without notice.

---

## Pocket Casts → Apple Podcasts

```sh
export POCKETCASTS_EMAIL="you@example.com"
export POCKETCASTS_PASSWORD="yourpassword"
export APPLE_BEARER_TOKEN="eyJhbGci..."
export APPLE_MEDIA_USER_TOKEN="0.Apgf..."
# Optional: KVS credentials for subscription writes + private-feed play state
export APPLE_KVS_DSID="12345678"
export APPLE_KVS_COOKIES="X-Dsid=12345678; mt-tkn-12345678=ABC...; ..."

# Dry-run first
podcast-migrate migrate --from pocketcasts --to podcasts --dry-run

# Live run (subscriptions + play state)
podcast-migrate migrate --from pocketcasts --to podcasts
```

See [Overcast → Apple Podcasts — Private and subscriber-feed episodes](#private-and-subscriber-feed-episodes-kvs) for the one-time Proxyman setup and credential capture steps. The Pocket Casts source provides complete play history — all episodes Pocket Casts has ever recorded, not just the most recent.

With KVS credentials set, any podcast in your Pocket Casts library not yet subscribed in Apple Podcasts is automatically subscribed before its episodes are synced. Without KVS credentials, only catalog-episode play state is written — subscriptions and private-feed episodes are skipped.

---

## Pocket Casts → Overcast

```sh
export POCKETCASTS_EMAIL="you@example.com"
export POCKETCASTS_PASSWORD="yourpassword"
export OVERCAST_EMAIL="you@example.com"
export OVERCAST_PASSWORD="yourpassword"

# Dry-run first
podcast-migrate migrate --from pocketcasts --to overcast \
  --play-state --dry-run

# Live run
podcast-migrate migrate --from pocketcasts --to overcast \
  --play-state
```

The Overcast match library is auto-fetched from your live account using the provided credentials — no `--overcast-source-opml` needed when Overcast is the destination. Any podcast not yet subscribed in Overcast is automatically subscribed before its episodes are written.

---

## Overcast → Pocket Casts

With Overcast credentials set, the extended OPML is fetched automatically — no manual export needed:

```sh
export OVERCAST_EMAIL="you@example.com"
export OVERCAST_PASSWORD="yourpassword"
export POCKETCASTS_EMAIL="you@example.com"
export POCKETCASTS_PASSWORD="yourpassword"

# Dry-run first
podcast-migrate migrate --from overcast --to pocketcasts \
  --play-state --dry-run

# Live run
podcast-migrate migrate --from overcast --to pocketcasts \
  --play-state
```

Or supply a manually-downloaded OPML with `--overcast-source-opml ~/Downloads/overcast.opml` if you prefer.

---

## OPML — export and import

### Export to OPML

```sh
# Apple Podcasts → OPML (subscriptions only)
podcast-migrate migrate --from podcasts --to opml \
  --opml-out ~/Desktop/my-podcasts.opml

# Apple Podcasts → OPML with play state (extended format, compatible with Overcast import)
podcast-migrate migrate --from podcasts --to opml \
  --opml-out ~/Desktop/my-podcasts.opml --play-state

# Pocket Casts → OPML with complete play history
export POCKETCASTS_EMAIL="you@example.com"
export POCKETCASTS_PASSWORD="yourpassword"

podcast-migrate migrate --from pocketcasts --to opml \
  --opml-out ~/Desktop/pocketcasts-export.opml --play-state
```

Pass `--pc-include-unsubscribed` to also include play history for podcasts you've since unsubscribed from.

### Import from OPML

```sh
# Import subscriptions into Overcast
podcast-migrate migrate --from opml --to overcast \
  --opml-file ~/Downloads/export.opml \
  --overcast-out ~/Desktop/overcast-import.opml

# Sync play state from an extended OPML into Pocket Casts
export POCKETCASTS_EMAIL="you@example.com"
export POCKETCASTS_PASSWORD="yourpassword"

podcast-migrate migrate --from opml --to pocketcasts \
  --opml-file ~/Downloads/export.opml --play-state
```

The OPML provider reads both standard OPML (subscriptions only) and the extended format with per-episode play state, as produced by Overcast's export and by `--to opml --play-state`.

---

## Export and import JSON

Snapshot your library as a portable JSON file:

```sh
# Print to stdout
podcast-migrate export --from podcasts

# Save to file
podcast-migrate export --from podcasts --out ~/Desktop/my-library.json

# Export from Overcast
podcast-migrate export --from overcast \
  --overcast-source-opml ~/Downloads/overcast.opml \
  --out ~/Desktop/overcast-library.json
```

Import from a JSON snapshot:

```sh
podcast-migrate import --to overcast \
  --in ~/Desktop/my-library.json \
  --overcast-out ~/Desktop/overcast-import.opml
```

---

## Mark a single episode as played (Overcast)

```sh
export OVERCAST_EMAIL="you@example.com"
export OVERCAST_PASSWORD="yourpassword"

podcast-migrate mark-played --url https://overcast.fm/+pGPCM1nmo
```

The episode URL is the share link from the Overcast app (Share Episode → Copy Link).

---

## Watch Apple Podcasts for live changes

`observe` polls the Apple Podcasts SQLite database and prints every play-state change in real time. Useful for debugging or understanding exactly what Apple Podcasts writes when you mark an episode:

```sh
podcast-migrate observe

# Filter to a specific show
podcast-migrate observe --podcast "fresh air"
```

Press Ctrl-C to stop.

---

## Common patterns

### Conflict resolution

When both sides have a play state for the same episode, `--conflict` controls which wins:

| Value | Behaviour |
|---|---|
| `furthest` *(default)* | Most progress wins; played beats in-progress beats unplayed |
| `source` | Source state always wins |
| `target` | Destination state is never overwritten |

### Feed mapping

Use `--feed-map` to explicitly remap a source feed URL to a destination feed URL when the automatic title-based matching isn't sufficient:

```sh
podcast-migrate migrate --from podcasts --to overcast \
  --play-state \
  --feed-map 'https://subscriber.example.com/feed=https://public.example.com/feed'
```

### Logging

`--log-file` writes a per-episode CSV for auditing:

```sh
podcast-migrate migrate --from podcasts --to overcast \
  --play-state --log-file ~/Desktop/sync-log.csv
```

Columns: `status`, `podcast`, `episode`, `pub_date`, `source_state`, `target_state`, `note`.

### Rate limiting

The default inter-request delay is 1 second. Increase it with `--request-delay` if you hit rate limits:

```sh
podcast-migrate migrate --from podcasts --to overcast \
  --play-state --request-delay 2s
```

See [Commands](commands.md) for the full flag reference.
