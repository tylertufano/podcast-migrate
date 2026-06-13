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

This direction writes play state via the Apple Podcasts web API, syncing to **all your Apple devices** (iPhone, iPad, Mac, podcasts.apple.com) automatically — no iCloud delay, no need to open the app.

### Step 1 — Get your Apple tokens (one-time)

1. Open [podcasts.apple.com](https://podcasts.apple.com) in your browser and sign in
2. Open DevTools (⌥⌘I in Safari, F12 in Chrome) → **Network** tab
3. Mark any episode as played in the web UI
4. Find the `PUT` request to `amp-api.podcasts.apple.com/v1/me/playback/positions/...`
5. Copy two header values from that request:
   - `Authorization` — everything after `Bearer ` (a long JWT string)
   - `media-user-token` — the full value of this header

The Bearer token expires in ~90 days; re-capture it the same way if you get `401` errors. The `media-user-token` lasts longer but also expires eventually.

### Step 2 — Export your Overcast library

Sign in to `overcast.fm`, go to **Account → Export OPML**, and download the extended OPML file.

### Step 3 — Run the migration

```sh
export APPLE_BEARER_TOKEN="eyJhbGci..."
export APPLE_MEDIA_USER_TOKEN="0.Apgf..."

# Dry-run first
podcast-migrate migrate --from overcast --to podcasts \
  --overcast-source-opml ~/Downloads/overcast.opml \
  --play-state --dry-run

# Live run
podcast-migrate migrate --from overcast --to podcasts \
  --overcast-source-opml ~/Downloads/overcast.opml \
  --play-state
```

Or pass tokens as flags instead of env vars:

```sh
podcast-migrate migrate --from overcast --to podcasts \
  --overcast-source-opml ~/Downloads/overcast.opml --play-state \
  --apple-bearer-token "eyJhbGci..." \
  --apple-media-user-token "0.Apgf..."
```

**Scope:** Only episodes indexed in the Apple Podcasts catalog (public RSS feeds) can be marked this way. Private or unlisted feeds without a catalog entry are skipped and reported.

### Limit to specific podcasts

`--podcast` works with any migration direction — pass a case-insensitive substring of the podcast title:

```sh
# Single podcast
podcast-migrate migrate --from overcast --to podcasts \
  --overcast-source-opml ~/Downloads/overcast.opml \
  --play-state --podcast "fresh air"

# Multiple podcasts
podcast-migrate migrate --from overcast --to podcasts \
  --overcast-source-opml ~/Downloads/overcast.opml \
  --play-state --podcast "fresh air" --podcast "planet money"

# From a file (one title word or phrase per line)
podcast-migrate migrate --from overcast --to podcasts \
  --overcast-source-opml ~/Downloads/overcast.opml \
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

# Dry-run first
podcast-migrate migrate --from pocketcasts --to podcasts \
  --play-state --dry-run

# Live run
podcast-migrate migrate --from pocketcasts --to podcasts \
  --play-state
```

See [Overcast → Apple Podcasts](#overcast--apple-podcasts) for the one-time Apple token capture step. The Pocket Casts source provides complete play history — all episodes Pocket Casts has ever recorded, not just the most recent.

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

Export your Overcast library first from `overcast.fm/account/export_opml/extended`, then:

```sh
export OVERCAST_EMAIL="you@example.com"
export OVERCAST_PASSWORD="yourpassword"
export POCKETCASTS_EMAIL="you@example.com"
export POCKETCASTS_PASSWORD="yourpassword"

# Dry-run first
podcast-migrate migrate --from overcast --to pocketcasts \
  --overcast-source-opml ~/Downloads/overcast.opml \
  --play-state --dry-run

# Live run
podcast-migrate migrate --from overcast --to pocketcasts \
  --overcast-source-opml ~/Downloads/overcast.opml \
  --play-state
```

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
