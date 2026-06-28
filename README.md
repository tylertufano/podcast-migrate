# podcast-migrate

A command-line tool for migrating podcast subscriptions and episode play state between **Apple Podcasts**, **Overcast**, and **Pocket Casts** — bidirectionally, with conflict resolution and idempotent repeat runs.

**[Full documentation →](https://tylertufano.github.io/podcast-migrate/)**

## How it works

podcast-migrate reads your library directly from the source app's local data or API, merges it with whatever the destination already knows, and writes the result. Each podcast service is an interchangeable adapter behind a common `Provider` interface, so adding new services doesn't require changes to the core migration logic.

Episode matching uses a cascade of up to six strategies (GUID → feed URL + pub date → feed URL + fuzzy title → fuzzy episode title + calendar day → podcast title + pub date → podcast title + fuzzy title) with automatic subscriber feed remapping — so paid-tier feeds like "Fresh Air Plus" route correctly to their public equivalents without any manual configuration. Each provider implements the strategies relevant to its data model; deviations are intentional and documented in `internal/migrate.MatchStrategy`.

## Supported providers

| Provider | Read subscriptions | Read play state | Write subscriptions | Write play state |
|---|:---:|:---:|:---:|:---:|
| Apple Podcasts | ✅ | ✅ | ✅ (KVS¹ → syncs to all devices) | ✅ (web API + KVS² or KVS-only³ → syncs to all devices) |
| Overcast | ✅ | ✅ | ✅ (API when credentials set⁴; or OPML export via `--overcast-out`) | ✅ (unofficial web API) |
| Pocket Casts | ✅ | ✅ complete history | ✅ (auto on play-state write⁴) | ✅ (unofficial web API) |
| OPML | ✅ | ✅ (extended format) | ✅ | ✅ (extended format) |

¹ Apple subscription writes require KVS credentials (`APPLE_KVS_DSID` + `APPLE_KVS_COOKIES`) captured via Proxyman. Subscriptions are written automatically during a play-state migration, or can be written standalone with `--only-subscriptions` (also requires KVS credentials).
² **Web API + KVS (recommended)**: Bearer token + `media-user-token` handle public-catalog episodes via `amp-api`; KVS handles private/subscriber-feed episodes. Public feeds resolve immediately without waiting for local indexing.
³ **KVS-only**: Set only `APPLE_KVS_DSID` + `APPLE_KVS_COOKIES` — no web API tokens needed. All episodes sync via KVS. Pre-existing subscriptions resolve immediately from the local SQLite DB; newly subscribed feeds wait for Apple Podcasts to index them first.
⁴ Overcast subscriptions use the unofficial web API when credentials are set (`OVERCAST_EMAIL` + `OVERCAST_PASSWORD`). `--only-subscriptions` with credentials subscribes programmatically without writing play state. Provide `--overcast-out` instead for an OPML file for manual import (no credentials needed). Private/subscriber feeds are always collected in a skipped-feeds OPML regardless of path. Default request delay is 5 s for `--only-subscriptions` and 3 s for `--play-state`; override with `--request-delay`.

## Platform support

All providers that use HTTP APIs (Overcast, Pocket Casts, OPML) work on macOS, Linux, and Windows. The Apple Podcasts provider has two read paths:

- **KVS-only** (all platforms) — reads subscriptions and play state directly from Apple's iCloud KVS, with episode metadata fetched from RSS. Activated automatically when `APPLE_KVS_DSID` + `APPLE_KVS_COOKIES` are set; takes precedence over SQLite on macOS as well.
- **SQLite** (macOS fallback) — reads from the local `MTLibrary.sqlite` database. Used only when KVS credentials are not configured.

Writing to Apple Podcasts (play state + subscriptions) always requires macOS.

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

Reading the Apple Podcasts database requires **Full Disk Access** for your terminal app on macOS Ventura and later (System Settings › Privacy & Security › Full Disk Access).

## Quick start

```sh
# Full Apple Podcasts → Overcast migration (recommended two-pass approach)
# Pass 1: generate subscription OPML (includes private feeds), then import
#         via Overcast → Settings → Import OPML
podcast-migrate migrate --from podcasts --to overcast \
  --overcast-out ~/Desktop/import.opml

# Pass 2: after importing and setting Download → Off in Overcast,
#         sync play state; --subscribed-only skips feed search for any podcasts
#         not yet subscribed in Overcast
export OVERCAST_EMAIL="you@example.com"
export OVERCAST_PASSWORD="yourpassword"
podcast-migrate migrate --from podcasts --to overcast --play-state --subscribed-only

# Single-pass API subscribe + play state (slower due to per-podcast rate limiting;
# private feeds are skipped to a separate OPML for manual import)
export OVERCAST_EMAIL="you@example.com"
export OVERCAST_PASSWORD="yourpassword"
podcast-migrate migrate --from podcasts --to overcast --play-state

# Sync play state Apple Podcasts → Overcast (incremental, last 48 hours)
podcast-migrate migrate --from podcasts --to overcast \
  --play-state --since 48h

# Sync play state Overcast → Apple Podcasts (syncs to iPhone, iPad, Mac)
# Option A: web API + KVS (recommended — public feeds resolve without waiting)
export OVERCAST_EMAIL="you@example.com"
export OVERCAST_PASSWORD="yourpassword"
export APPLE_BEARER_TOKEN="eyJhbGci..."
export APPLE_MEDIA_USER_TOKEN="0.Apgf..."
export APPLE_KVS_DSID="12345678"          # required for private feeds + subscriptions
export APPLE_KVS_COOKIES="X-Dsid=..."
podcast-migrate migrate --from overcast --to podcasts --play-state

# Option B: KVS-only (simpler — no web API tokens needed)
export OVERCAST_EMAIL="you@example.com"
export OVERCAST_PASSWORD="yourpassword"
export APPLE_KVS_DSID="12345678"
export APPLE_KVS_COOKIES="X-Dsid=..."
podcast-migrate migrate --from overcast --to podcasts --play-state
```

See [Usage](https://tylertufano.github.io/podcast-migrate/usage) for step-by-step guides for every supported migration direction.

## Known issues

**Unofficial APIs** — Overcast and Pocket Casts write paths use undocumented internal endpoints that those services haven't publicly committed to. They work as of the current release but may break without notice. Always `--dry-run` before a live migration.

**Apple token expiry** — the Bearer token for the web API path is a short-lived JWT (~90 days). Re-capture it from browser DevTools if you get `401` errors. If you'd rather avoid managing these tokens, use KVS-only mode (just `APPLE_KVS_DSID` + `APPLE_KVS_COOKIES`). See [Providers](https://tylertufano.github.io/podcast-migrate/providers) for details on both modes.

**Apple subscriber and internal feeds** — `internal://` feeds (Apple-exclusive shows with no public RSS) are excluded from all exports. Subscriber and private feeds (NPR+, Slate+ via supportingcast.fm, etc.) are included and routed correctly at each destination.

When reading via `KVSReader`, some subscriptions carry a KVS feed URL that differs from the iTunes canonical URL (e.g. a subscriber edition like `feeds.simplecast.com/54nAGcIl` vs. the iTunes canonical `feeds.simplecast.com/Sl5CSM3S`). For these, `KVSReader` fetches both RSS feeds and classifies the relationship before deciding which URL to export:

| Class | Condition | Default URL (`subscriber` mode) |
|---|---|---|
| `private-auth` | KVS RSS inaccessible or empty | iTunes canonical |
| `public-equivalent` | KVS RSS accessible, identical content and depth to iTunes | iTunes canonical |
| `public-archive` | KVS RSS accessible, same episodes in iTunes window but deeper archive | KVS URL |
| `public-subscriber` | KVS RSS has episodes absent from iTunes in the same date window | KVS URL |

Control this behaviour with `--private-feed`:
- `subscriber` (default) — automatic detection as above
- `public` — always use the iTunes canonical URL
- `kvs` — always use the KVS URL as-is
- `custom` — prompt interactively for each mismatched feed

When migrating *to* Apple Podcasts with KVS credentials set, private feeds are subscribed directly via KVS and can coexist alongside an existing public subscription with separate episode history. When migrating *to* Overcast, private feeds are collected in a skipped-feeds OPML for manual import via Add Feed → URL — Overcast has no programmatic subscribe path for non-iTunes feeds. When migrating *to* Pocket Casts, private and subscriber feeds are submitted via the `add_feed_url` API, which can index arbitrary RSS URLs; feeds that fail (typically auth-gated URLs PC's backend cannot reach) are collected in a skipped-feeds OPML for manual import via Add Podcast → Add via podcast URL. Without KVS credentials, private-feed subscriptions and Apple Podcasts episode writes are skipped.

**`--apple-all-play-state`** — by default, `KVSReader` only fetches RSS for currently-subscribed feeds; play state from feeds you've since unsubscribed is omitted. Pass `--apple-all-play-state` to also fetch RSS for unsubscribed feeds and include their episodes in the migration. Useful when consolidating play history after a podcast moved to a new feed URL and you re-subscribed under the new one.

**Apple KVS session expiry** — the iTunes Store session cookies required for KVS writes must be captured from a live Apple Podcasts request via a proxy tool (Proxyman). They last days to weeks before expiring; re-capture them the same way when you see `status=1198` errors. See [Providers](https://tylertufano.github.io/podcast-migrate/providers) for the capture steps.

**`--since` is Apple-only** — delta sync currently only filters the Apple Podcasts SQLite reader. Overcast and Pocket Casts sources always read the full play history.

**Overcast migration order** — subscribe to podcasts *before* writing play state to avoid Overcast auto-downloading episodes it will immediately mark played. The recommended approach is a two-pass migration: (1) generate a subscription OPML with `--overcast-out` and import it — faster than the API path and includes private feeds the API cannot subscribe; (2) set Download → Off in Overcast; (3) run `--play-state --subscribed-only` to sync play state, skipping feed search for any podcasts not covered by the OPML. See [Providers](https://tylertufano.github.io/podcast-migrate/providers) for the full workflow.

## Future work

### Reliability and correctness

**`--since` for Overcast and Pocket Casts sources** — Overcast's OPML export includes a `userUpdatedDate` attribute per episode; Pocket Casts' `sync/update` endpoint accepts a real `lastModified` timestamp. Wiring `--since` into these source paths would make incremental syncs faster on all three platforms.

### Features

**Continuous sync / observe-and-write** — the `observe` command detects Apple Podcasts SQLite changes in real time. Extending it (or adding a `daemon` subcommand) to trigger incremental writes on each detected change would enable true background sync without manual `--since` runs.

**`--since` as a persistent state file** — an `--incremental` flag could write a state file after each successful run and use the stored timestamp as the `--since` value for the next run automatically.

**Progress reporting** — the Overcast episode ID resolution step can take several minutes for large libraries with no output. A `--verbose` flag or built-in progress counter would substantially improve the experience.

**Credential config file** — a `--creds-file` option (or auto-loading from `~/.config/podcast-migrate/credentials`) would reduce setup friction when migrating between multiple providers.

**Apple token auto-extraction** — the Bearer token must currently be captured manually from browser DevTools. The Podcasts app may cache credentials in the macOS Keychain; automatic extraction and renewal would eliminate the only manual step in the Apple write path.

**Overcast episode cache targeted invalidation** — `--clear-episode-cache` drops all cached episode IDs. A `--invalidate-podcast "title"` option would allow selective cache busting for one podcast without a full re-fetch.

### Additional providers

The `Provider` interface makes adding new services straightforward:
- **Castro** — reads locally from `Castro.sqlite` (similar approach to the Apple Podcasts reader)
- **Spotify Podcasts** — Spotify has a listening history API; would require OAuth credentials

### Infrastructure

**WebAPIWriter testability** — `WebAPIWriter` has no unit tests because `CatalogClient.FindEpisode` requires live Apple tokens. Extracting a `catalogFinder` interface would allow testing retry logic, skip-reason checks, dry-run, and `ForceUpdate` with an `httptest.Server` stub.

**Packaged release** — signed macOS binary via GitHub Actions, distributed through Homebrew.

## Tests

```sh
go test ./...
```

All tests run offline — no live API credentials required. Provider-to-API interactions use `httptest.NewServer` fake servers.

| Package | Coverage | Notes |
|---|---|---|
| `internal/model` | 100% | |
| `internal/sync` | ~93% | |
| `internal/opml` | ~95% | |
| `internal/migrate` | ~89% | |
| `internal/itunes` | ~86% | |
| `internal/pocketcasts` | ~65% | Web API write path partially live-only |
| `internal/overcast` | ~53% | Web API write path partially live-only |
| `internal/apple` | ~27% | `kvs.go` write path requires live credentials; offline-testable functions (`private_feed.go`, `rss.go`, `sqlite.go`) are individually at 85–100% |

See [Testing](https://tylertufano.github.io/podcast-migrate/testing) for a per-file breakdown and prioritised gap list.

## Project structure

```
cmd/            CLI entry points (migrate, export, import, mark-played, observe)
internal/
  model/        Shared types: Library, Podcast, EpisodeState
  provider/     Provider interface and WriteOptions
  migrate/      Shared utilities: normalisation, fuzzy title matching, skip-reason logic, MatchStrategy
  httputil/     Shared HTTP retry: RateLimitError, TransientError, RetryFunc (used by all write providers)
  apple/        Apple Podcasts adapter (SQLite read; catalog API + web API write)
  overcast/     Overcast adapter (OPML read/write; unofficial web API)
  pocketcasts/  Pocket Casts adapter (web API read/write)
  opml/         Standard and extended OPML read/write
  sync/         Merge engine, conflict resolution, automatic feed remapping
main.go
```

See [Architecture](https://tylertufano.github.io/podcast-migrate/architecture) for a full walkthrough of the data model and sync engine.
