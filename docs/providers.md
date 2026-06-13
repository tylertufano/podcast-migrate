---
layout: default
title: Providers
nav_order: 4
---

# Providers

## Apple Podcasts (`internal/apple`)

### Capabilities

| Operation | Supported |
|---|---|
| Read subscriptions | ✓ |
| Read play state | ✓ |
| Write subscriptions | ✗ (no public API) |
| Write play state | ✓ (web API, when credentials are set) |

### Reading — SQLiteReader

`SQLiteReader` opens `MTLibrary.sqlite` in read-only WAL mode. The default path:

```
~/Library/Group Containers/
  243LU875E5.groups.com.apple.podcasts/Documents/MTLibrary.sqlite
```

**Podcast query** (`ZMTPODCAST`): subscribed podcasts with `http`/`https` feed URLs, excluding:
- `internal://` feeds (Apple-exclusive, no public RSS)
- Feeds containing `/eyJ` (JWT authentication tokens embedded in the URL — subscriber feeds from NPR Plus, Slate Plus via supportingcast.fm, etc. that are valid only for one Apple account and break Overcast's OPML importer)

Apple adds cache-buster `?t=` query parameters to stored feed URLs. These are stripped before the URL is returned so they don't break feed matching in other apps.

**Episode query** (`ZMTEPISODE`): episodes with any evidence of prior listening:

```sql
WHERE (ZPLAYSTATE != 0 OR ZPLAYHEAD > 0 OR ZPLAYCOUNT > 0 OR ZLASTDATEPLAYED IS NOT NULL)
```

**Play state determination** (in priority order):

1. `ZPLAYHEAD > 0` → `PlayStateInProgress` with the stored position
2. `ZPLAYSTATE = 2` AND trusted (not auto-marked) → `PlayStatePlayed`
3. `ZPLAYSTATE = 1` with no playhead → `PlayStatePlayed` (started, no position stored)
4. `ZPLAYCOUNT > 0` AND `ZPLAYSTATESOURCE = 0` → `PlayStatePlayed` (iCloud synced without state)
5. `ZLASTDATEPLAYED` set with a non-auto source (1, 3, or 4) AND `ZPLAYCOUNT = 0` → `PlayStatePlayed` (played on another device, only timestamp synced)
6. Otherwise: **skip** (no reliable evidence of genuine playback)

**Trust logic for `ZPLAYSTATESOURCE`:**

| Value | Meaning | Trusted? |
|---|---|---|
| 1 | Manually marked by user in UI | ✓ Always |
| 2 | Auto-marked when a newer episode arrived | ✗ Never |
| 3 | Listened to completion | ✓ Only when `ZPLAYCOUNT > 0` OR `ZLASTDATEPLAYED` is set |
| 4 | Synced from another device | ✓ Always |
| 6 | Default/initial (also bulk auto-marks) | ✗ Never |
| 0 | Unset | Conditional (see case 4 and 5 above) |

Source 3 is conditional because Apple Podcasts Subscription (PSUB/PLUS) back-catalog auto-marks also use source=3 with no play count or date — identical to a genuine completion.

**Delta sync** (`--since`): filters by three timestamp columns:
- `ZPLAYSTATELASTMODIFIEDDATE` — updated on state transitions
- `ZPLAYHEADLASTMODIFIEDDATE` — updated on every playhead advance (critical for resumed in-progress episodes; probed at runtime since it may not exist on all macOS versions)
- `ZLASTDATEPLAYED` — completion / device sync

**Schema compatibility**: `ZDURATION` was removed in macOS 27. The reader probes with `PRAGMA table_info(ZMTEPISODE)` and substitutes `NULL` when absent.

**OPML fallback**: if SQLite is inaccessible (permissions, path doesn't exist, read error), the provider falls back to an Apple Podcasts OPML export. OPML provides subscriptions only — no play state.

### Writing — WebAPIWriter

Writes play state via `amp-api.podcasts.apple.com`. This approach writes to Apple's backend, which syncs to all Apple devices (iPhone, iPad, Mac) through PodcastContentService.

**Two tokens are required:**

| Token | Header | How to obtain |
|---|---|---|
| `bearerToken` | `Authorization: Bearer <jwt>` | App-level JWT, ~90 day expiry, same for all users. Open `podcasts.apple.com`, mark any episode played, copy from DevTools → Network → Authorization header. |
| `mediaUserToken` | `media-user-token` | User-specific identifier for the Apple Account. Same request, copy `media-user-token` header. |

**Episode ID resolution** (two-step, performed by `CatalogClient`):

1. **iTunes Search API** (`itunes.apple.com/search?media=podcast&entity=podcast&term=<title>`) → finds the podcast's `collectionId`, matching first by feed URL then by podcast title (with Plus-normalization fallback)

2. **amp-api catalog** (`amp-api.podcasts.apple.com/v1/catalog/us/podcasts/<id>/episodes`) → paginates all episodes (100 per page) and builds an in-memory index using four strategies:
   - `feeddate:<normFeedURL>|<RFC3339>` — feed URL + exact pub date
   - `feedtitle:<normFeedURL>|<lowercaseTitle>` — feed URL + episode title
   - `poddate:<lowercasePodTitle>|<RFC3339>` — podcast title + exact pub date (cross-feed)
   - `podtitle:<lowercasePodTitle>|<lowercaseEpisodeTitle>` — podcast title + episode title (cross-feed)

**Write flow per episode:**
1. `FindEpisode` → resolve to Apple catalog `int64` episode ID
2. `getServerPosition` (GET `/v1/me/playback/positions/podcast-episodes/<id>`) → check current server state
3. If server is already at or beyond the desired state (and `ForceUpdate` is false) → skip
4. `markPosition` (PUT `/v1/me/playback/positions/podcast-episodes/<id>`) → write the position. `completed=true, positionMs=0` for fully played; `completed=false, positionMs=N` for in-progress.

**Retry logic**: `markPosition` retries up to 3 times (2s → 4s → 8s exponential backoff) on 5xx and network errors. 4xx client errors are not retried.

---

## Overcast (`internal/overcast`)

### Capabilities

| Operation | Supported |
|---|---|
| Read subscriptions | ✓ (from OPML export) |
| Read play state | ✓ (from extended OPML) |
| Write subscriptions | ✓ (generates OPML import file) |
| Write play state | ✓ (unofficial web API, when credentials set) |

### Reading — OPMLReader

Parses Overcast's extended OPML export (`overcast.fm/account/export_opml/extended`). The extended format includes per-episode `<outline>` elements with:
- `overcastId` attribute → stored as `EpisodeState.GUID` (used as the `numericId` for `set_progress` calls)
- `userUpdatedDate` → pub date
- `played` → play state
- `progress` → playback position in seconds

### Writing — Subscriptions

Generates an OPML file at `--overcast-out` that the user imports via **Overcast → Settings → Import OPML**. This is the only supported subscription write path (Overcast has no API for programmatic subscription management).

### Writing — Play State (unofficial API)

**Authentication**: POST to `overcast.fm/login` with email, password, and `then=account`. Session cookie is stored in the HTTP client's cookie jar.

**Episode index** is built from the matching OPML (either `--overcast-match-opml` or auto-fetched from the live account). Two keys per episode:
- `feeddate:<normFeedURL>|<RFC3339>` — primary (pub date + feed URL)
- `feedtitle:<normFeedURL>|<fuzzyNormTitle>` — fallback (fuzzy title + feed URL)

`overcastId` (from the OPML's `overcastId` attribute) is stored as the `numericId` used in `set_progress` calls. The index also carries `currentState` and `currentPos` (from the OPML export) for skip-reason evaluation.

**Extended matching** (`augmentIndexFromPodcastPages`): resolves episode IDs for Apple episodes not in the Overcast OPML (episodes never opened in Overcast). Three steps:

1. **`/podcasts` page** — fetch all subscribed podcast page URLs in one request (replacing N per-podcast search calls). Matches by podcast title (including Plus-normalization). Falls back to `search_autocomplete` JSON API for unsubscribed podcasts, then calls `SubscribeToPodcast` (idempotent; no-op if already subscribed).

2. **Podcast listing pages** (`/itunes<ID>/<slug>` or `/p<ID>-<hash>`) — extract `PodcastEpisodeListing` per episode: `OvercastURL` (`/+HASH`), `DateStr` (YYYY-MM-DD), `Title`, and opportunistically `NumericID` (when `data-item-id` is already present on the cell anchor, saving a per-episode fetch).

3. **Episode pages** (`/+HASH`) — for entries where the listing page didn't provide a numeric ID, fetch the episode player page and extract `data-item-id` or parse it from a `set_progress` URL on the page. This step runs with a bounded worker pool (5 concurrent workers) with a shared rate-limiter ticker.

**Episode ID cache** (`overcast-episode-ids.json` in `os.UserCacheDir()/podcast-migrate/`):
- Keyed by Overcast episode URL (`https://overcast.fm/+HASH`)
- Stores: `id` (numeric ID), `t` (timestamp for maxAge), `ws` (last-written PlayState), `wp` (last-written position in seconds)
- Persistent across runs — avoids re-fetching episode pages for already-resolved episodes
- `--clear-episode-cache` discards all entries; `--episode-cache-max-age` treats old entries as stale
- Written state (`ws`/`wp`) is used by `SkipReason` on subsequent runs to prevent re-writing the same play state

**`SetProgress` endpoint**: `POST overcast.fm/podcasts/set_progress/<numericId>` with form values `p=<positionSeconds>`, `speed=0`, `v=0`. Use `p=2147483647` (`PlayedSentinel`, INT32_MAX) to mark as played.

**Retry logic**: both `SetProgress` and listing-page fetches implement separate retry budgets for 429 (rate limit, with `Retry-After` header support) and 5xx/network errors (exponential backoff: 2s → 4s → 8s for write calls; 30s → 60s → 120s for page fetches). An auth-failure abort fires immediately if the very first write call is redirected to the login page.

**Rate limiting guards**: The listing-page loop, per-episode worker pool, and search calls all share the `requestDelay` timer to pace requests at ≤ 1 per second by default, staying well within Overcast's observed limits.

---

## Pocket Casts (`internal/pocketcasts`)

### Capabilities

| Operation | Supported |
|---|---|
| Read subscriptions | ✓ |
| Read play state | ✓ (complete history via `sync/update`) |
| Write subscriptions | ✓ |
| Write play state | ✓ |

### Reading

**Authentication**: POST to `api.pocketcasts.com/user/login` → JWT token used in all subsequent requests.

**Subscription list** (`/user/podcast/list`): returns all subscribed podcasts with UUID, title, author. Some entries lack feed URLs (private/subscriber feeds). Missing URLs are batch-resolved via the export service (`/user/podcast/list/export/by/uuid`), which returns the originally-subscribed URL including private feed URLs (e.g. Slate Plus, NPR Plus subscriber feeds).

**Complete play history** (primary path):
1. `POST /user/sync/update` with `lastModified=1` (protobuf endpoint used by Pocket Casts mobile apps) → returns all episodes PC has ever interacted with, including `playing_status` and `played_up_to`
2. For each podcast with at least one played/in-progress episode: paginate `/user/podcast/episodes` CDN endpoint to get episode titles and pub dates

**Fallback** (when `sync/update` fails): `GET /user/in_progress` + `GET /user/history` (capped at ~100 played episodes).

**`--pc-include-unsubscribed`**: when set, attempts to recover feed URLs for podcasts in sync history that are no longer subscribed, via the PC CDN or the iTunes Search API.

### Writing — Subscriptions

Each new podcast is resolved from its RSS feed URL to a Pocket Casts UUID via the refresh API (`refresh.pocketcasts.com/podcast/full/<feedURL>`), then subscribed via `POST /user/podcast/subscribe`. Lookup failures fall back to title-based matching (NormalizePlusTitle, then fuzzy).

### Writing — Play State

**Phase A1** — Index from in-progress episodes (`GET /user/in_progress`): carries real, current play-state values.

**Phase A2** — Index from play history (`GET /user/history`): recently completed episodes. Note: `isDeleted=true` is NOT skipped here because Pocket Casts uses that flag to mean "removed from queue", not "deleted episode".

**Phase A_sync** — Full play-state overlay from `sync/update`: used to overlay accurate play state onto CDN-fetched episodes in Phase B, since the CDN returns `PlayingStatus=0` for all episodes.

**Phase B** — Per-podcast episode fetch for unmatched source episodes:
1. **Pass 1 (authenticated)**: `GET /user/podcast/episodes/<podcastUUID>` → all episodes with real play state
2. **Pass 2 (CDN)**: `GET /podcast/full/<podcastUUID>?page=N` → paginated episode list (no play state); overlaid with Phase A_sync state

Index key priority:
1. `feeddate:<normFeedURL>|<RFC3339>` — feed URL + exact pub date
2. `feedtitle:<normFeedURL>|<fuzzyNormTitle>` — feed URL + fuzzy title
3. `titledate:<fuzzyNormTitle>|<YYYY-MM-DD>` — title + calendar date only (cross-podcast fallback for network cross-posting)

Podcast UUID resolution when the source feed URL doesn't match any PC subscription:
1. PC refresh API (`ResolveFeedToPodcastUUID`)
2. `NormalizePlusTitle` exact title match against subscription list
3. `FuzzyNormalizeTitle` exact then word-prefix match

**`UpdateEpisodeProgress`**: `POST api.pocketcasts.com/user/playback/sync_progress` with `{ episode_uuid, podcast_uuid, playing_status, played_up_to, duration }`.

**Important**: Pocket Casts has no per-episode read-back API to check current state before writing. The skip-reason check relies on the Phase A/A_sync index values. `--force-update` has no effect on the Pocket Casts provider.

---

## OPML (`internal/opml`)

### Capabilities

**Source provider** (from `--opml-file`):

| Operation | Supported |
|---|---|
| Read subscriptions | ✓ |
| Read play state | ✓ (if extended OPML format) |
| Write | ✗ |

**Output provider** (from `--opml-out`):

| Operation | Supported |
|---|---|
| Read | ✗ |
| Write subscriptions | ✓ |
| Write play state | ✓ (extended OPML format) |

### Format

Standard OPML: `<outline type="rss">` elements with `xmlUrl` (feed URL) and `text` (title) attributes.

Extended OPML (Overcast format): each podcast `<outline>` contains child `<outline>` elements for episodes, with `overcastId`, `played`, `progress`, `userUpdatedDate` attributes.

The output provider always writes extended OPML when `--play-state` is set; this makes the output directly compatible with Overcast's "Import OPML" feature.
