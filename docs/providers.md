---
layout: default
title: Providers
nav_order: 5
---

# Providers

## Apple Podcasts (`internal/apple`)

### Capabilities

| Operation | Supported |
|---|---|
| Read subscriptions | ✓ |
| Read play state | ✓ |
| Write subscriptions | ✓ (KVS, when `APPLE_KVS_DSID` + `APPLE_KVS_COOKIES` are set) |
| Write play state | ✓ (web API + KVS, or KVS-only — see below) |

### Reading — SQLiteReader

`SQLiteReader` opens `MTLibrary.sqlite` in read-only WAL mode. The default path:

```
~/Library/Group Containers/
  243LU875E5.groups.com.apple.podcasts/Documents/MTLibrary.sqlite
```

**Podcast query** (`ZMTPODCAST`): subscribed podcasts with `http`/`https` feed URLs, excluding:
- `internal://` feeds (Apple-exclusive, no public RSS)

Subscriber feeds with private JWT-authenticated URLs (e.g. NPR Plus via `wbez.plus.npr.org`, supportingcast.fm) are included. The SQLiteReader returns these feeds using the subscriber URL stored in the local database. When running via `KVSReader` (see below), these URLs are automatically replaced with the canonical public feed URL from the iTunes Store so that destination apps subscribe to the correct public listing.

Apple adds cache-buster `?t=` query parameters to stored feed URLs. These are stripped before the URL is returned so they don't break feed matching in other apps.

**Episode query** (`ZMTEPISODE` LEFT JOIN `ZMTUPPMETADATA`): all episodes with any evidence of prior listening, including episodes whose only play record is in the KVS mirror:

```sql
LEFT JOIN ZMTUPPMETADATA u ON u.ZMETADATAIDENTIFIER = e.ZMETADATAIDENTIFIER
WHERE (ZPLAYSTATE != 0 OR ZPLAYHEAD > 0 OR ZPLAYCOUNT > 0 OR ZLASTDATEPLAYED IS NOT NULL
       OR u.ZHASBEENPLAYED = 1 OR u.ZBOOKMARKTIME > 0)
```

**Play state determination — Phase 1: `ZMTUPPMETADATA` (primary)**

`ZMTUPPMETADATA` is the authoritative source when a row exists for an episode. Apple Podcasts reads play state from this table for its UI, not from `ZMTEPISODE.ZPLAYSTATE`. The two tables are updated through independent write paths and can permanently disagree even on a fully synced device — about 22% of interacted-with episodes have a `ZMTUPPMETADATA` row. `ZHASBEENPLAYED` is never NULL when a row exists, so its validity serves as a reliable row-existence sentinel.

| Condition | Result |
|---|---|
| Row exists AND `ZHASBEENPLAYED = 1` | `PlayStatePlayed` |
| Row exists AND `ZBOOKMARKTIME > 0` | `PlayStateInProgress` with the stored position |
| Row exists AND `ZHASBEENPLAYED = 0` AND `ZBOOKMARKTIME = 0` | **skip** — KVS says unplayed; overrides `ZMTEPISODE` regardless of `ZPLAYSTATE` |
| No row | Fall through to Phase 2 |

**Play state determination — Phase 2: `ZMTEPISODE` heuristics (fallback)**

For episodes with no `ZMTUPPMETADATA` row, play state is inferred from `ZMTEPISODE` fields in priority order:

1. `ZPLAYHEAD > 0` → `PlayStateInProgress` with the stored position
2. `ZPLAYSTATE = 2` AND trusted source → `PlayStatePlayed`
3. `ZPLAYSTATE = 1` with no playhead → `PlayStatePlayed` (started, no position stored)
4. `ZPLAYCOUNT > 0` AND `ZPLAYSTATESOURCE = 0` → `PlayStatePlayed` (iCloud synced count without state)
5. `ZLASTDATEPLAYED` SET AND `ZPLAYSTATESOURCE = 3` AND `ZPLAYCOUNT > 0` → `PlayStatePlayed` (completion on mobile device; completion flag did not sync back to Mac but count and timestamp did — see below)
6. `ZLASTDATEPLAYED` SET AND `ZPLAYSTATESOURCE = 4` → `PlayStatePlayed` (synced from another device; `ZPLAYCOUNT` may be 0 or >0 depending on how much iCloud propagated — both indicate a genuine play event)
7. Otherwise: **skip** (no reliable evidence of genuine playback)

**Trust logic for `ZPLAYSTATESOURCE`:**

| Value | Meaning | Trusted? |
|---|---|---|
| 1 | Manually marked by user in UI | ✓ Always |
| 2 | Auto-marked when a newer episode arrived | ✗ Never |
| 3 | Listened to completion | ✓ Only when `ZPLAYCOUNT > 0` AND `ZLASTDATEPLAYED` is set (case 5) |
| 4 | Synced from another device | ✓ When `ZLASTDATEPLAYED` is set (case 6) |
| 6 | Default/initial (also bulk auto-marks) | ✗ Never |
| 0 | Unset | Conditional (see cases 4 and 6 above) |

Source 3 is conditional because Apple Podcasts Subscription (PSUB/PLUS) back-catalog auto-marks also use source=3 with no play count or date — identical to a genuine completion.

Source 4 is trusted when `ZLASTDATEPLAYED` is set because that timestamp confirms a real play event was propagated from another device. `ZPLAYCOUNT` being 0 vs. >0 reflects how much data iCloud propagated, not whether the play actually happened.

**iCloud sync gap (case 5)**: When an episode is listened to completion on iPhone or iPad, the mobile device records the event (incrementing `ZPLAYCOUNT` and setting `ZLASTDATEPLAYED`) but `ZPLAYSTATE` often remains `0` on the Mac because iCloud does not always propagate the completion flag. Apple also clears `ZLASTDATEPLAYED` when the user manually marks an episode as unplayed (while retaining `ZPLAYCOUNT` and `ZPLAYSTATESOURCE=3`). Requiring `ZLASTDATEPLAYED` to be set makes "completed but not synced" (date present) reliably distinguishable from "completed then manually unplayed" (date cleared).

**Live KVS read** (`APPLE_KVS_DSID` + `APPLE_KVS_COOKIES`): when KVS credentials are set and Apple Podcasts is the migration source, the reader calls `getAll(com.apple.upp)` on `bookkeeper.itunes.apple.com` and uses the server-side play state instead of the local `ZMTUPPMETADATA` cache. This gives a more authoritative result when the Mac SQLite database is stale (e.g. the Mac was not opened between a play event and the migration run).

`getAll` returns at most 5,000 entries (hard server-side cap, most-recently-modified first). On a large library, this covers all episodes with any recent listening activity. The log reports how many records were fetched and how many matched episodes in the local library.

The live KVS read has two phases:
1. **Main query** (same as without live KVS) — episodes with local play evidence. For each episode that has a `ZMETADATAIDENTIFIER`, the server-side `HasBeenPlayed` / `BookmarkTimeSec` overrides local `ZMTUPPMETADATA`. Episodes not present in the live KVS response fall through to `ZMTEPISODE` heuristics.
2. **Second pass** — for metadataIdentifiers in the live KVS that were not returned by the main query (no local play evidence at all), `ZMTEPISODE` is queried individually to fetch episode metadata. This picks up plays that exist only on the server and have not yet propagated to the local SQLite cache.

The second pass is skipped when `--since` is active, since delta sync is scoped to recently-modified episodes and the live KVS carries no per-episode modification timestamp.

**Delta sync** (`--since`): filters by three timestamp columns:
- `ZPLAYSTATELASTMODIFIEDDATE` — updated on state transitions
- `ZPLAYHEADLASTMODIFIEDDATE` — updated on every playhead advance (critical for resumed in-progress episodes; probed at runtime since it may not exist on all macOS versions)
- `ZLASTDATEPLAYED` — completion / device sync

**Schema compatibility**: `ZDURATION` was removed in macOS 27. The reader probes with `PRAGMA table_info(ZMTEPISODE)` and substitutes `NULL` when absent.

**OPML fallback**: if SQLite is inaccessible (permissions, path doesn't exist, read error), the provider falls back to an Apple Podcasts OPML export. OPML provides subscriptions only — no play state.

### Reading — KVSReader (cross-platform, no SQLite)

`KVSReader` reads subscriptions and play state entirely from Apple's iCloud KVS — no local SQLite database required. This enables Apple Podcasts migrations from **non-macOS platforms** (Linux, Windows) and from macOS machines where the Apple Podcasts database is inaccessible.

Activated automatically when `APPLE_KVS_DSID` + `APPLE_KVS_COOKIES` are set and SQLite is not available. On macOS, SQLite takes precedence; set both KVS and SQLite credentials to get the live KVS overlay on top of SQLite.

**Data sources:**
- `com.apple.podcasts` KVS domain — subscription list (feed URL, title, iTunes Store ID) and per-feed episode identity (GUID → `metadataIdentifier`)
- `com.apple.upp` KVS domain — per-episode play state (played, bookmark position, timestamp of last change)
- RSS feeds — episode titles, pub dates, durations (fetched concurrently, up to 8 parallel)

**iTunes canonical URL resolution**: for every catalog subscription (`PodcastPID > 0`), `KVSReader` performs a batched lookup against the iTunes Store API (`itunes.apple.com/lookup`) to resolve the canonical public feed URL. The iTunes ID is stored in `model.Podcast.ITunesID` and used by the Overcast writer to subscribe directly via `/itunes{ID}` without a `search_autocomplete` round-trip.

**Subscriber URL preservation and `IsPrivate` flag**: when the KVS feed URL differs from the iTunes canonical URL (e.g. `slateprivate.supportingcast.fm/content/eyJ…` vs. `feeds.slate.com/…`), the podcast is a subscriber or private edition. In this case the KVS subscriber URL is exported as `pod.FeedURL` — not replaced by the canonical URL — and `pod.IsPrivate` is set to `true`. Feeds with no `PodcastPID` (self-hosted, unindexed) are also marked `IsPrivate`. Only public/catalog feeds (KVS URL matches iTunes canonical) get the canonical URL substitution.

Destination providers use `IsPrivate` to route feeds correctly without manual configuration: the Apple KVS writer accepts the private feed URL directly and can subscribe it alongside an existing public subscription; the Overcast writer collects private feeds into a skipped-feeds OPML for manual import via Add Feed → URL (Overcast has no programmatic subscribe path for non-iTunes feeds).

**`internal://` feeds**: Apple-exclusive shows with no public RSS are excluded from output and counted in `lib.SkippedInternalPodcasts`.

**Episode coverage**: `com.apple.upp` is capped at 5,000 entries (most-recently-modified first). On a large library this covers all recently active episodes. The UPP cap is shared with the live KVS overlay on the SQLite path.

**Unsubscribed feeds**: `com.apple.podcasts` retains play state for feeds the user has since unsubscribed. These appear in `lib.Podcasts` (without a subscription flag) and their episodes are included if play state exists.

### Writing — Two modes

Play state writes work in one of two modes depending on which credentials are provided:

#### Mode A — Web API + KVS (recommended)

Set `APPLE_BEARER_TOKEN` + `APPLE_MEDIA_USER_TOKEN` (required) and optionally `APPLE_KVS_DSID` + `APPLE_KVS_COOKIES`:

| Operation | Path | Endpoint |
|---|---|---|
| Play state — public/catalog episode | `WebAPIWriter` | `amp-api.podcasts.apple.com` |
| Play state — private/subscriber-feed episode | `KVSWriter` (`com.apple.upp`) | `bookkeeper.itunes.apple.com` |
| Subscriptions | `KVSWriter` (`com.apple.podcasts`) | `bookkeeper.itunes.apple.com` |

Public-catalog episodes resolve via Apple's global catalog API without needing the local Apple Podcasts app to index the feed first. KVS credentials are optional in this mode — without them, private-feed play state and subscription writes are skipped.

#### Mode B — KVS-only

Set only `APPLE_KVS_DSID` + `APPLE_KVS_COOKIES` (no web API tokens required):

| Operation | Path | Endpoint |
|---|---|---|
| Play state — all episodes (public and private) | `KVSWriter` (`com.apple.upp`) | `bookkeeper.itunes.apple.com` |
| Subscriptions | `KVSWriter` (`com.apple.podcasts`) | `bookkeeper.itunes.apple.com` |

All episodes use the same KVS path. Episodes from podcasts that were already subscribed in Apple Podcasts resolve immediately from the local SQLite database (`ZMTEPISODE.ZMETADATAIDENTIFIER`). Episodes from newly subscribed feeds wait for Apple Podcasts to index the feed before their `metadataIdentifier` is available — the same deferred retry loop used for private feeds in Mode A.

**Trade-off:** KVS-only requires no web API token management, but large migrations where many feeds are newly subscribed will take longer as Apple Podcasts indexes each one. The web API mode resolves all catalog episodes immediately regardless of local indexing status.

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

**Retry logic**: `markPosition` retries up to 3 times on both 429 (rate limit, with `Retry-After` header support) and 5xx/network errors (exponential backoff: 2s → 4s → 8s). 4xx client errors are not retried. The iTunes Search API and `amp-api` catalog paging calls use the same shared retry budget via `internal/httputil.RetryFunc`.

### Writing — KVSWriter

`KVSWriter` handles two distinct responsibilities via Apple's key-value store at `bookkeeper.itunes.apple.com`: play state for private/subscriber-feed episodes, and subscription management. Both use the same credentials and session.

**Two credentials are required:**

| Variable | Description | How to obtain |
|---|---|---|
| `APPLE_KVS_DSID` | iTunes Store account DSID (numeric ID) | Copy the `X-Dsid` header value from any `bookkeeper.itunes.apple.com` request in Proxyman |
| `APPLE_KVS_COOKIES` | Full `Cookie` header from an active iTunes Store session | Copy the `Cookie` header value from the same request |

**Capturing credentials with the capture script:**

The repo includes `scripts/capture-kvs-creds.sh`, which automates the full capture using Proxyman's CLI (`proxyman-cli export-log`):

```sh
# One-time Proxyman setup (only needed once):
# 1. Install Proxyman from https://proxyman.io
# 2. Trust its root certificate
# 3. Add bookkeeper.itunes.apple.com to SSL Proxying

# Then capture credentials automatically:
eval $(./scripts/capture-kvs-creds.sh)

# Or write to .creds file:
./scripts/capture-kvs-creds.sh --write
source .creds
```

The script checks Proxyman's current session for `bookkeeper.itunes.apple.com` traffic. If none exists, it triggers a sync automatically. When Apple Podcasts is not already running, the script disables the Proxyman proxy first, launches Podcasts, waits for it to initialize, then re-enables the proxy — Podcasts must start without the proxy active or it cannot connect to Apple's servers during launch and will not perform a KVS sync afterward. The proxy is always restored on exit. No manual copy-paste required.

To capture manually without the script: open Proxyman, find any `bookkeeper.itunes.apple.com` request, and copy the `Cookie` and `X-Dsid` header values.

#### KVS play state — `com.apple.upp`

Writes play state for private and subscriber-feed episodes (where `ZSTORETRACKID = 0` in the local SQLite).

This is the same domain Apple Podcasts itself uses for cross-device sync of episodes not in the public catalog — private RSS feeds, subscriber feeds (NPR Plus, Slate Plus, supportingcast.fm, etc.), and any feed requiring authentication.

**Domain and key format:**

- Domain: `com.apple.upp`
- Key: `ZMTEPISODE.ZMETADATAIDENTIFIER` (32-char hex string, unique per episode)
- Value: binary plist `{bktm, hbpl, plct, tstm}` compressed with raw DEFLATE

| Field | Type | Meaning |
|---|---|---|
| `bktm` | float64 | Bookmark time in seconds (`0.0` = fully played, `N > 0` = in-progress position) |
| `hbpl` | bool | Has been played |
| `plct` | int | Play count |
| `tstm` | float64 | Timestamp — seconds since CoreData epoch (2001-01-01 UTC) |

**Episode identifier lookup — two sources:**

The `metadataIdentifier` required as the KVS key is not derivable from episode metadata (GUID, title, pub date) — it must come from Apple. `KVSWriter` tries two sources in order:

1. **`com.apple.podcasts` play state cache** (fast, no SQLite): `getAll(com.apple.podcasts)` returns `playState:<feedURL>` keys for every subscribed feed. Each entry contains the `metadataIdentifier` for every known episode in that feed. This is checked first and covers the majority of episodes on subscribed feeds.
2. **Local SQLite** (`ZMTUPPMETADATA.ZMETADATAIDENTIFIER`): fallback for episodes not yet indexed in the play state cache (e.g. very new episodes, or feeds subscribed but never opened on the Mac).

**Request flow:**

1. `getAll(com.apple.podcasts)` — fetches play state and subscription data for all subscribed feeds (see Subscriptions below). Also populates the `metadataIdentifier` lookup cache.
2. `getAll(com.apple.upp)` — fetches current server-side versions **and values** for all episode keys. Used as `base-version` in `putAll` calls; the values are also used for the skip-reason check below.
3. **Skip-reason check** — before writing each episode, the server-side value from step 2 is lazily decoded (DEFLATE decompress → binary plist → `{bktm, hbpl}`). If `hbpl=true` for a "played" episode, or `bktm ≥ desired − 5s` for an in-progress episode, the episode is logged as `skipped` (`already synced via KVS`) and no `putAll` entry is generated. Pass `--force-update` to bypass.
4. `putAll(com.apple.upp)` — sends all episodes that passed the skip check in a single batched request (chunked at 25 per call). Each entry includes:
   - `key`: the `metadataIdentifier`
   - `base-version`: server-side version from step 2 (stale local SQLite `Z_OPT` values are always overwritten)
   - `value`: the DEFLATE-compressed binary plist

**Why `base-version` must come from `getAll`:** the server enforces optimistic concurrency — if the submitted `base-version` doesn't match the server's current version, `putAll` returns `status=1198`. Because other devices write to KVS independently, the local SQLite `Z_OPT` value is typically stale.

**Re-runs are safe:** the skip-reason check makes KVS writes idempotent. Re-running a migration after episodes have already been synced produces no writes — only `skipped` log entries.

#### KVS subscriptions — `com.apple.podcasts`

Manages subscriptions via the `podcastSubscriptions-2012-09-04` key in the `com.apple.podcasts` domain. This is the same mechanism Apple Podcasts uses for cross-device subscription sync.

**Domain and key format:**

- Domain: `com.apple.podcasts`
- Key: `podcastSubscriptions-2012-09-04`
- Value: binary plist `{2: [{subscription entries}], DataVersion: 2}` compressed with raw DEFLATE

Each subscription entry contains:

| Field | Type | Notes |
|---|---|---|
| `uuid` | string | Random UUID v4 assigned at subscribe time |
| `feedURL` | string | RSS feed URL |
| `title` | string | Podcast title |
| `subscribed` | bool | `true` = active subscription, `false` = unsubscribed |
| `addedDate` | NSDate | When the subscription was created |
| `lastTouchDate` | NSDate | Last modification time |
| `updatedDate` | NSDate | Last update time |
| `podcastPID` | integer | iTunes Store podcast ID (absent for private feeds) |
| `storeCollectionId` | integer | iTunes Store collection ID (absent for private feeds) |
| `darkCount` | integer | Unplayed episode badge count |
| `playbackNewestToOldest` | bool | Episode playback order preference |
| `showTypeSetting` | integer | Show type setting |

**Subscription write flow:**

When migrating to Apple Podcasts, `KVSWriter` automatically subscribes to any feed in the source library that is not yet in the destination subscription list:

1. `getAll(com.apple.podcasts)` — fetch current subscription list and all per-feed play state (also used for `metadataIdentifier` lookup). The server-returned version of `podcastSubscriptions-2012-09-04` is cached as `base-version` for the subsequent write.
2. For each feed not already subscribed, subscribe with dedup:
   - **Private/subscriber feeds** (`IsPrivate=true` or detected by title/domain heuristic) are processed first (stable sort). A private feed is skipped if the exact URL is already subscribed, or if any existing subscription with the same normalized title is also a private type. If a matching **public** subscription already exists, both are kept — Apple Podcasts supports coexisting public and private editions with separate episode history. A note with the Apple Podcasts deep link (`https://podcasts.apple.com/podcast/id{PID}`) is printed to assist navigation.
   - **Public/catalog feeds** are looked up via `itunes.FindByHints` to resolve the canonical URL and iTunes Store ID, then subscribed with `PodcastPID` set so Apple Podcasts can show the podcast's store page.
3. `putAll(com.apple.podcasts)` — write the full updated subscription list. The `base-version` from step 1 is required; `KVSWriter` refuses to write if the version is unknown (guards against overwriting the remote subscription list with a partial or empty local snapshot).

Auto-subscribe runs before the play state write, so that newly subscribed feeds are visible in Apple Podcasts before episode state is applied.

**Session token behaviour:** the iTunes Store session cookie is refreshed by the server on each successful `getAll`/`putAll` exchange. The tool uses a cookie jar to carry forward `Set-Cookie` updates automatically. The underlying session is valid for days to weeks.

**Status codes:**

| Status | Meaning |
|---|---|
| `0` | Success |
| `-2` | Not authenticated (invalid or expired session) |
| `-4` | Session expired — re-capture credentials |
| `1198` | Base-version conflict or session expired — re-capture credentials |

---

## Overcast (`internal/overcast`)

### Capabilities

| Operation | Supported |
|---|---|
| Read subscriptions | ✓ (from OPML — explicit file or auto-fetched) |
| Read play state | ✓ (from extended OPML — explicit file or auto-fetched) |
| Write subscriptions | ✓ (API when credentials set; or OPML import file via `--overcast-out`) |
| Write play state | ✓ (unofficial web API, when credentials set) |

### Reading — source OPML

The Overcast provider reads from an extended OPML export (`overcast.fm/account/export_opml/extended`). The extended format includes per-episode `<outline>` elements with:
- `overcastId` attribute → stored as `EpisodeState.GUID` (used as the `numericId` for `set_progress` calls)
- `userUpdatedDate` → pub date
- `played` → play state
- `progress` → playback position in seconds

**Auto-fetch**: when `--overcast-source-opml` is not set and Overcast credentials are provided, the provider fetches the extended OPML directly from `overcast.fm/account/export_opml/extended` after login and caches it at:

```
~/Library/Caches/podcast-migrate/overcast-source.opml   (macOS)
~/.cache/podcast-migrate/overcast-source.opml            (Linux)
```

The cache is valid for 24 hours. Use `--clear-source-opml-cache` to force a fresh fetch, or `--overcast-save-source-opml [path]` to save a copy (defaults to `~/Downloads/overcast.opml` when given without a value).

### Writing — Subscriptions

Two paths are supported, selected automatically:

**API subscribe** (when `OVERCAST_EMAIL` + `OVERCAST_PASSWORD` are set and `--overcast-out` is not provided): subscribes each podcast programmatically via the same unofficial web API used by the play-state write path. This is the path taken by `--only-subscriptions` when credentials are configured.

For each podcast in the source library:
- Already subscribed on Overcast (matched by normalised title from `/podcasts`) → skip
- Private/subscriber-edition feed (`IsPrivate` or `model.IsSubscriberFeed`) → collected into skipped-feeds OPML (Overcast has no API path for non-iTunes feeds)
- iTunes ID available (from source library or `itunes.FindByHints`) → `GET /itunes{ID}` then `POST /podcasts/add/{overcastID}` (`SubscribeToPodcast`)
- Otherwise → `search_autocomplete` → `POST /podcasts/add/{overcastID}` (`AddPodcast`)

**Default request delay**: `--only-subscriptions` defaults to **4 s** between operations (vs 3 s for `--play-state`). Each subscribe makes two Overcast requests (a page GET to resolve the internal podcast ID, then a POST to add it); both are individually rate-limited using the configured delay. Use `--request-delay` to override both defaults. The `--only-subscriptions` path fires subscribes back-to-back, so it needs more headroom than the play-state path where subscribes are naturally spread across hours of episode fetches.

**OPML export** (when `--overcast-out` is provided): generates an OPML file that the user imports via **Overcast → Settings → Import OPML**. No credentials required. Use this path when you want to review subscriptions before committing, or when credentials are not available.

#### Recommended full-library migration — OPML first

For a complete library migration, the OPML export path is faster and more complete than the API subscribe path:

- **Faster**: Overcast's bulk OPML import handles all subscriptions in one operation, bypassing the per-podcast request loop and its rate-limiting constraints.
- **More complete**: The OPML includes private and subscriber-edition feeds (NPR+, Slate+, etc.) directly, so they are imported as-is. The API path cannot subscribe non-iTunes feeds and routes them to a separate skipped-feeds OPML for manual follow-up.

Recommended order for a full migration:

1. **Generate a subscription OPML** — run with `--overcast-out ~/Desktop/import.opml`. No credentials required.
2. **Import in Overcast** — Settings → Import OPML. Overcast subscribes all feeds including private ones.
3. **Set Download to Manual** — Settings → Default Settings → Download → **Off**. This prevents auto-download when play state is written next.
4. **Sync play state** — run with `--play-state --subscribed-only`. Credentials required. `--subscribed-only` skips episode resolution for any feeds not yet subscribed in Overcast, which avoids re-running feed search logic for podcasts that were not in the OPML.
5. **Re-enable automatic downloads** (optional).

The `--only-subscriptions` API path is more convenient when you want a fully automated single-step run, accept the rate-limiting pauses, and have few or no private feeds.

### Writing — Play State (unofficial API)

**Authentication**: POST to `overcast.fm/login` with email, password, and `then=account`. Session cookie is stored in the HTTP client's cookie jar.

**Episode index** is built from the matching OPML (either `--overcast-match-opml` or auto-fetched from the live account). Two keys per episode:
- `feeddate:<normFeedURL>|<RFC3339>` — primary (pub date + feed URL)
- `feedtitle:<normFeedURL>|<fuzzyNormTitle>` — fallback (fuzzy title + feed URL)

`overcastId` (from the OPML's `overcastId` attribute) is stored as the `numericId` used in `set_progress` calls. The index also carries `currentState` and `currentPos` (from the OPML export) for skip-reason evaluation.

**Extended matching** (`augmentIndexFromPodcastPages`): resolves episode IDs for source episodes not already in the Overcast OPML index. Three steps:

**Pre-seeding (Overcast → Overcast short-circuit)**: before any HTTP is attempted, if the source episode's `GUID` is a pure-numeric Overcast ID (i.e. the source is an Overcast OPML export), the episode is pre-seeded into the destination index directly using that ID. This means Overcast → Overcast migrations skip the listing-page and episode-page fetches entirely for episodes with known IDs.

1. **`/podcasts` page** — fetch all subscribed podcast page URLs in one request (replacing N per-podcast search calls). Matches by podcast title (including Plus-normalization). For unsubscribed podcasts, three paths in priority order:
   - **Private/subscriber-edition feeds** (`IsPrivate=true` in the source library, or a title/domain heuristic match via `model.IsSubscriberFeed`): bypassed before any subscribe attempt. These feeds have no iTunes Store listing that Overcast can resolve; subscribing the public counterpart would be wrong. They are collected directly into the skipped-feeds OPML for manual import via Overcast → Add Feed → URL.
   - **iTunes ID known** (source is `KVSReader` or the source library contains `model.Podcast.ITunesID`): constructs the Overcast page URL directly as `/itunes{ID}` and calls `SubscribeToPodcast` — no `search_autocomplete` round-trip, no title-matching ambiguity.
   - **iTunes ID not known** (self-hosted, no iTunes listing): falls back to `search_autocomplete` JSON API, then calls `POST /podcasts/add/<overcastId>` to subscribe (bypasses page-scraping caching bugs). Podcasts that `search_autocomplete` cannot resolve are collected and written to `--overcast-skipped-opml` at the end of the run.

2. **Podcast listing pages** (`/itunes<ID>/<slug>` or `/p<ID>-<hash>`) — extract `PodcastEpisodeListing` per episode: `OvercastURL` (`/+HASH`), `DateStr` (YYYY-MM-DD), `Title`, and opportunistically `NumericID` (when `data-item-id` is already present on the cell anchor, saving a per-episode fetch). When a podcast publishes multiple episodes on the same calendar day, all candidates are tried and the one whose title is compatible with the source episode is selected.

3. **Episode pages** (`/+HASH`) — for entries where the listing page didn't provide a numeric ID, fetch the episode player page and extract `data-item-id` or parse it from a `set_progress` URL on the page. This step runs with a bounded worker pool (5 concurrent workers) with a shared rate-limiter ticker.

**Private / custom feed OPML** (`--overcast-skipped-opml`): podcasts that cannot be subscribed programmatically (no iTunes ID in `search_autocomplete`) are written to an OPML file at the end of the run. Import via **Overcast → Settings → Import OPML**. Default path: `skipped-private-feeds.opml` in the working directory; pass the flag with a value to override.

**Episode ID cache** (`overcast-episode-ids.json` in `os.UserCacheDir()/podcast-migrate/`):
- Keyed by Overcast episode URL (`https://overcast.fm/+HASH`)
- Stores: `id` (numeric ID), `t` (timestamp for maxAge), `ws` (last-written PlayState), `wp` (last-written position in seconds)
- Persistent across runs — avoids re-fetching episode pages for already-resolved episodes
- `--clear-episode-cache` discards all entries; `--episode-cache-max-age` treats old entries as stale
- Written state (`ws`/`wp`) is used by `SkipReason` on subsequent runs to prevent re-writing the same play state

**`SetProgress` endpoint**: `POST overcast.fm/podcasts/set_progress/<numericId>` with form values `p=<positionSeconds>`, `speed=0`, `v=0`. Use `p=2147483647` (`PlayedSentinel`, INT32_MAX) to mark as played.

**Retry logic**: both `SetProgress` and listing-page fetches implement separate retry budgets for 429 (rate limit, with `Retry-After` header support) and 5xx/network errors (exponential backoff: 2s → 4s → 8s for write calls; 30s → 60s → 120s for page fetches). An auth-failure abort fires immediately if the very first write call is redirected to the login page.

**Rate limiting guards**: All Overcast requests share the `requestDelay` timer. The default is **3 s** for `--play-state` and **4 s** for `--only-subscriptions`; override with `--request-delay`. When the retry budget is exhausted on a persistent 429, the migration pauses and prompts interactively:

```
overcast: persistent rate limiting — still getting 429 after retries.
Waiting 60s as requested by server...
Continue? [Y/n] (current delay 2s; press Enter to also increase to 2.5s):
```

Press **Enter** to continue with a +0.5 s delay increase, **y** to continue without changing the delay, or **n** to abort cleanly.

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

**Skip-reason check**: before writing, the provider compares the desired play state against the indexed current state (`entry.currentState`/`entry.currentPos` populated from Phase A1, A2, or the sync overlay). If Pocket Casts is already at or ahead of the desired state, the episode is logged as `already_played` or `already_ahead` and no API call is made — matching the Overcast write path's behaviour. Pass `--force-update` to bypass this check and write unconditionally.

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
