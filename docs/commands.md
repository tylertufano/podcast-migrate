---
layout: default
title: Commands
nav_order: 4
---

# Commands

## migrate

The primary command. Reads a source library, merges it with the destination's current state, and writes the result.

```
podcast-migrate migrate --from <src> --to <dst> [flags]
```

**Supported provider names:** `podcasts` / `apple`, `overcast`, `pocketcasts` / `pc`, `opml`

### Required flags

| Flag | Description |
|---|---|
| `--from` | Source provider |
| `--to` | Destination provider |

### Scope flags

| Flag | Default | Description |
|---|---|---|
| `--play-state` | false | Also write episode play state (requires credentials) |
| `--only-subscriptions` | false | Skip play state entirely |
| `--dry-run` | false | Log what would happen without writing anything |

### Source / destination configuration

| Flag | Description |
|---|---|
| `--sqlite` | Path to `MTLibrary.sqlite` (auto-detected when omitted) |
| `--opml-fallback` | Apple Podcasts OPML export (fallback when SQLite is inaccessible) |
| `--overcast-source-opml` | Overcast extended OPML export. Optional when Overcast credentials are set — the extended OPML is fetched automatically and cached for 24 h (see `--clear-source-opml-cache`). Required when using a specific snapshot (e.g. restoring from an old export). |
| `--overcast-match-opml` | Overcast OPML used for write-side episode matching (optional; auto-fetched after login when omitted) |
| `--overcast-out` | Output path for generated Overcast import OPML |
| `--opml-file` | Source OPML path (required when `--from opml`) |
| `--opml-out` | Output OPML path (required when `--to opml`) |

### Credentials

| Flag | Env variable | Description |
|---|---|---|
| `--overcast-email` | `OVERCAST_EMAIL` | Overcast account email |
| `--overcast-password` | `OVERCAST_PASSWORD` | Overcast account password |
| `--apple-bearer-token` | `APPLE_BEARER_TOKEN` | Apple Podcasts web API Bearer JWT |
| `--apple-media-user-token` | `APPLE_MEDIA_USER_TOKEN` | Apple Podcasts user-specific token |
| `--pocketcasts-email` | `POCKETCASTS_EMAIL` | Pocket Casts account email |
| `--pocketcasts-password` | `POCKETCASTS_PASSWORD` | Pocket Casts account password |

**Obtaining Apple tokens:** Open `podcasts.apple.com` in a browser, open DevTools → Network, mark any episode as played, then copy the `Authorization: Bearer …` header value and the `media-user-token` header value from the resulting request.

### Conflict resolution

| Flag | Default | Description |
|---|---|---|
| `--conflict` | `furthest` | `furthest` (most progress wins), `source` (source always wins), `target` (destination always wins) |

### Filtering

| Flag | Description |
|---|---|
| `--podcast "word"` | Limit play-state writes to podcasts whose title contains this word (case-insensitive, repeatable) |
| `--podcast-list /path` | File with one title word per line; combined with `--podcast` |
| `--subscribed-only` | Only sync play state for podcasts already subscribed at the destination |
| `--strict-feed-match` | Disable cross-feed title fallback strategies (only use feed-URL-anchored matching) |

### Delta sync

`--since` limits the Apple Podcasts source to episodes whose play state changed after the given cutoff. It matches any of three timestamp columns:
- `ZPLAYSTATELASTMODIFIEDDATE` — state transitions (unplayed → in-progress → played)
- `ZPLAYHEADLASTMODIFIEDDATE` — playhead advances (resumed in-progress episodes), when present
- `ZLASTDATEPLAYED` — completion or cross-device sync

### Apple Podcasts source flags

These flags apply when `--from podcasts` (KVS read path).

| Flag | Default | Description |
|---|---|---|
| `--apple-all-play-state` | false | Include play state for feeds you are no longer subscribed to. By default only current subscriptions have their RSS fetched, so unsubscribed episodes have no title and are omitted. Useful when consolidating history after a podcast moved to a new feed URL. |
| `--private-feed` | `subscriber` | Strategy for feeds where the Apple subscription URL differs from the iTunes canonical URL. See [Providers](providers) for classification details. Options: `subscriber` (auto-detect, keep KVS when it adds archive or subscriber value), `public` (always use iTunes canonical), `kvs` (always use KVS URL), `custom` (prompt interactively per feed, requires TTY). |
| `--since` | — | Delta sync cutoff. Only process episodes whose Apple play state changed after this point. Accepts a duration (`7d`, `24h`) or a date (`2026-06-01`, `2026-06-01T12:00`, RFC3339). See [Delta sync](#delta-sync). |

### Advanced

| Flag | Default | Description |
|---|---|---|
| `--request-delay` | 1s | Pause between consecutive API requests |
| `--title-match-tolerance` | 72h | Max pub-date gap for title-based episode matching |
| `--force-update` | false | Write source state even if destination is already ahead |
| `--episode-cache-max-age` | 0 (indefinite) | Treat Overcast episode ID cache entries older than this as stale |
| `--clear-episode-cache` | false | Discard and rebuild Overcast episode ID cache |
| `--clear-source-opml-cache` | false | Discard the cached Overcast source OPML and force a fresh download. Effective whenever Overcast is the source or destination and no explicit `--overcast-source-opml` path is given. |
| `--overcast-save-source-opml [path]` | — | Save a copy of the auto-fetched Overcast source OPML to this path. If given without a value, saves to `~/Downloads/overcast.opml`. |
| `--overcast-skipped-opml [path]` | — | When `--to overcast`: write an OPML file of podcasts that could not be auto-subscribed (private/custom feeds with no iTunes ID). If given without a value, writes to `skipped-private-feeds.opml` in the current directory. Import via **Overcast → Settings → Import OPML**. |
| `--pc-skipped-opml [path]` | — | When `--to pocketcasts`: write an OPML file of podcasts that could not be auto-subscribed (private/subscriber feeds whose URL the Pocket Casts `add_feed_url` API could not index). If given without a value, writes to `skipped-private-feeds.opml` in the current directory. Add each feed manually via **Add Podcast → Add via podcast URL**. |
| `--feed-map SRC=DST` | — | Remap a source feed URL to a destination feed URL (repeatable) |
| `--log-file /path` | — | Write per-episode CSV log (columns: status, podcast, episode, pub_date, source_state, target_state, note) |
| `--pc-include-unsubscribed` | false | When `--from pocketcasts`: include play history for unsubscribed podcasts |

### Examples

```bash
# Dry-run preview: Podcasts → Overcast subscriptions
podcast-migrate migrate --from podcasts --to overcast \
  --overcast-out ~/Desktop/import.opml --dry-run

# Full migration with play state, restricted to two shows
podcast-migrate migrate --from podcasts --to overcast \
  --overcast-source-opml ~/Downloads/overcast.opml \
  --overcast-out ~/Desktop/import.opml \
  --play-state --podcast "fresh air" --podcast "planet money"

# Delta sync last 48 hours
podcast-migrate migrate --from podcasts --to overcast \
  --overcast-source-opml ~/Downloads/overcast.opml \
  --play-state --since 48h

# Reverse sync: Overcast → Apple Podcasts (credentials auto-fetch the source OPML)
podcast-migrate migrate --from overcast --to podcasts --play-state

# Reverse sync: Overcast → Apple Podcasts (explicit OPML; useful for restoring from a snapshot)
podcast-migrate migrate --from overcast --to podcasts \
  --overcast-source-opml ~/Downloads/overcast.opml --play-state

# Pocket Casts → Overcast (full migration)
podcast-migrate migrate --from pocketcasts --to overcast \
  --overcast-out ~/Desktop/import.opml --play-state

# Use an explicit feed URL remapping for a subscriber feed
podcast-migrate migrate --from podcasts --to overcast \
  --overcast-source-opml ~/Downloads/overcast.opml --play-state \
  --feed-map 'https://feeds.apple.com/subscriber/abc=https://feeds.overcast.com/xyz'

# Write per-episode CSV log for auditing
podcast-migrate migrate --from podcasts --to overcast \
  --overcast-source-opml ~/Downloads/overcast.opml --play-state \
  --log-file ~/Desktop/sync-log.csv
```

---

## export

Reads a provider's library and serialises it to JSON (or stdout).

```
podcast-migrate export --from <src> [--out /path/to/file.json]
```

| Flag | Default | Description |
|---|---|---|
| `--from` | (required) | Source provider: `podcasts`, `overcast`, `opml` |
| `--out` | stdout | Output path; `-` for stdout |
| `--sqlite` | auto | Path to MTLibrary.sqlite |
| `--opml-fallback` | — | Apple Podcasts OPML fallback |
| `--overcast-source-opml` | — | Overcast extended OPML |
| `--opml-file` | — | Source OPML file path (when `--from opml`) |

The JSON output is a serialised `model.Library` and can be fed into the `import` command.

---

## import

Reads a JSON library export and writes it to a provider.

```
podcast-migrate import --to <dst> --in /path/to/file.json [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--to` | (required) | Destination provider: `overcast` |
| `--in` | (required) | Path to JSON library file |
| `--dry-run` | false | Preview without writing |
| `--only-subscriptions` | false | Import subscriptions only |
| `--overcast-out` | — | Path for generated Overcast import OPML |
| `--conflict` | `furthest` | Conflict resolution strategy |

---

## mark-played

Marks a single Overcast episode as played using its overcast.fm URL.

```
podcast-migrate mark-played --url https://overcast.fm/+pGPCM1nmo
```

| Flag | Description |
|---|---|
| `--url` | Overcast episode URL, e.g. `https://overcast.fm/+pGPCM1nmo` (required) |
| `--overcast-email` | Overcast account email (or `OVERCAST_EMAIL`) |
| `--overcast-password` | Overcast account password (or `OVERCAST_PASSWORD`) |
| `--request-delay` | Delay between API requests |

The command authenticates, fetches the episode's numeric ID from the episode page, and calls the `set_progress` endpoint with `PlayedSentinel` (2147483647).

---

## observe

Polls the Apple Podcasts SQLite database and prints every play-state change in real time. Useful for reverse-engineering what Apple Podcasts writes when the user marks an episode as played.

```
podcast-migrate observe [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--sqlite` | auto-detected | Path to MTLibrary.sqlite |
| `--podcast` | — | Case-insensitive substring filter for podcast title |
| `--episode` | — | Case-insensitive substring filter for episode title |
| `--interval` | 200 | Poll interval in milliseconds |

**What it watches:**
- `ZMTEPISODE` column changes (detected via `Z_OPT` version counter)
- New `ACHANGE` rows (CoreData persistent history)
- New `ATRANSACTION` rows (commit attribution, including bundle ID and process ID)
- `playState:<feedURL>` preference keys in `com.apple.podcasts.plist`

Run this command while Apple Podcasts is open, then mark an episode as played in the UI to see the exact sequence of writes Apple makes.

---

## version

```
podcast-migrate --version
```

The version string is injected at build time via `-ldflags`:
```
go build -ldflags="-X github.com/tyler/podcast-migrate/cmd.version=v0.12.0" .
```

Local builds without ldflags report `dev`.
