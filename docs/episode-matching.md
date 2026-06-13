---
layout: default
title: Episode Matching
nav_order: 6
---

# Episode Matching

Matching episodes across podcast platforms is the hardest part of this migration. Feed URLs may differ (http vs https, CDN redirects), pub dates may be off by hours or days (timezone differences, early-access windows), titles may include season markers or subscriber-only prefixes, and some podcasts appear under different names depending on whether the user has a paid subscription.

## Canonical Key Hierarchy

The sync engine matches episodes using a priority cascade. The first strategy to produce a match wins.

### Strategy 1: GUID

```
key = "guid:" + episode.GUID
```

An RSS `<guid>` is the most stable identifier. Used only when both sides have a GUID for the same episode. Not applicable for Overcast (which stores `overcastId` in the GUID field, not the RSS GUID).

### Strategy 2: Feed URL + Publication Date

```
key = "feeddate:" + normalizeFeedURL(feedURL) + "|" + pubDate.UTC().RFC3339
```

Both sides must agree on the feed URL (after normalization) and the exact UTC timestamp. This is the primary strategy for Overcast and Pocket Casts, where pub dates are usually reliable.

### Strategy 3: Feed URL + Title

```
key = "feedtitle:" + normalizeFeedURL(feedURL) + "|" + fuzzyNormalizeTitle(title)
```

Used when pub dates differ but the episode belongs to the same feed. `FuzzyNormalizeTitle` strips season markers (`S01`, `Season 1`) and punctuation, so "The Retrievals - Ep. 4" and "The Retrievals S01 - Ep. 4" produce the same key.

### Strategy 4: Cross-Feed (by normalised podcast title + date)

```
key = "xfeed:" + normalizePlusTitle(podcastTitle) + "|" + pubDate.UTC().date
```

Used in the sync engine's `merge()` when strategies 1–3 all fail. Matches episodes across different feed URLs when the podcast title normalises to the same base (e.g. "Fresh Air Plus" ↔ "Fresh Air"). Uses day-level date precision (not RFC3339) to tolerate timing differences between early-access and public RSS feeds.

A `±1-day` window is tried when exact date fails. Off-by-one matches require a fuzzy title agreement to prevent false positives.

When multiple destination episodes share the same podcast+date bucket (batch releases), `pickBestCrossFeedCandidate` selects the closest fuzzy title match.

---

## Provider-Specific Matching

Each provider adds its own episode-lookup index and uses the strategies relevant to its data format.

### Overcast index keys

| Key | Strategy | When used |
|---|---|---|
| `feeddate:<normURL>|<RFC3339>` | Feed URL + exact pub date | Primary |
| `feedtitle:<normURL>|<fuzzyTitle>` | Feed URL + fuzzy title | Fallback |

The Overcast OPML stores `overcastId` (a numeric string) as the GUID field. This value is used in `set_progress` calls. It is **not** the same as the RSS `<guid>`.

### Pocket Casts index keys

| Key | Strategy | When used |
|---|---|---|
| `feeddate:<normURL>|<RFC3339>` | Feed URL + exact pub date | Primary |
| `feedtitle:<normURL>|<fuzzyTitle>` | Feed URL + fuzzy title | Fallback |
| `titledate:<fuzzyTitle>|<YYYY-MM-DD>` | Title + calendar date (no feed URL) | Cross-podcast fallback |

The `titledate` key handles episodes that podcast networks cross-post to multiple feeds, where Apple and Pocket Casts attribute the episode to different shows.

### Apple catalog keys (web API write path)

| Strategy | Key | Flag |
|---|---|---|
| 1: feeddate | `feeddate:<normURL>|<RFC3339>` | Always tried |
| 2: feedtitle | `feedtitle:<normURL>|<title>` | Always tried (with date tolerance) |
| 3: poddate | `poddate:<podTitle>|<RFC3339>` | Skipped with `--strict-feed-match` |
| 4: podtitle | `podtitle:<podTitle>|<title>` | Skipped with `--strict-feed-match` |

---

## Feed URL Normalization

All matching uses `migrate.NormalizeFeedURL`:

```
- Lowercase scheme and host
- http → https (treated as equivalent)
- Strip trailing slash from path
- Drop URL fragment
- Preserve query parameters (some feeds use them as identity)
```

Apple's `?t=<timestamp>` cache-buster parameters are stripped **before** normalization (in the SQLite reader, where they are first encountered).

---

## Title Normalization

### `FuzzyNormalizeTitle`

Used for episode title matching within a feed:

1. Lowercase
2. Remove season markers: `S01`, `S1`, `Season 1`, `Season 01`
3. Remove apostrophes and typographic equivalents (so "O'Brien" = "OBrien")
4. Replace remaining non-alphanumeric characters with spaces
5. Collapse whitespace

**Examples:**
- `"The Retrievals - Ep. 4"` → `"the retrievals ep 4"`
- `"The Retrievals S01 - Ep. 4"` → `"the retrievals ep 4"`
- `"Conan O'Brien Needs a Friend"` → `"conan obrien needs a friend"`

### `NormalizePlusTitle`

Used for **podcast** title matching to bridge public and paid-tier feeds:

**Subscriber/private feed decorations** (stripped by index position so dynamic trailing text is also removed):
- `" - Subscriber Feed …"`
- `" - Member Feed …"`
- `" - Private Feed …"`
- `" - Premium Feed …"`
- `" (🔓)"` (standalone lock emoji suffix)

**Plus-tier suffixes:**
- `" Plus"` (e.g. "Fresh Air Plus" → "fresh air")
- `" +"` (e.g. "Planet Money +" → "planet money")
- `"+"` (e.g. "Planet Money+" → "planet money")

---

## Auto Feed Map

`buildAutoFeedMap` runs before `merge()` and derives feed URL remappings automatically by title-matching source podcasts against destination subscriptions:

**Purpose**: A user may have an Apple Podcasts Subscription feed (e.g. `internal://` scheme, or a subscriber JWT feed URL like `https://rss.nytimes.com/services/.../subscriber-feed/...`) that has no equivalent public RSS URL. If the user already subscribes to the analog feed on the destination app, we remap the source URL to the destination URL so downstream matching works.

**Algorithm**:

1. For each source podcast **not** already subscribed by feed URL on the destination, compute `fuzzyPodcastTitle(pod.Title)` = `FuzzyNormalizeTitle(NormalizePlusTitle(title))`
2. Pass 1: exact fuzzy-title match against destination subscription list
3. Pass 2: word-aligned prefix match (handles subtitle additions: "Crooked City" ↔ "Crooked City: Dixon, IL")

**`titleHasWordPrefix`** ensures that "pod save america" matches "pod save america plus" (true prefix) but NOT "breaking news from pod save america" (suffix, not prefix).

**Collision guards**:
1. Skip if the destination URL is already a direct source feed URL (avoids collapsing two different podcasts)
2. If two source podcasts both title-match to the same destination URL, suppress **both** remappings (the provider's extended matching will resolve each podcast independently via its RSS URL)

---

## Conflict Resolution

When both source and destination have a play state for the same episode, `resolveConflict` applies the configured strategy:

| Strategy | `--conflict` flag | Behaviour |
|---|---|---|
| `FurthestWins` | `furthest` (default) | Played beats in-progress beats unplayed; for in-progress, higher position wins |
| `SourceWins` | `source` | Source state always wins |
| `TargetWins` | `target` | Destination state always wins |

For cross-feed matches (strategy 4), the destination episode's identity fields (GUID, FeedURL, Title, PubDate) are always preserved even when the source state wins. This ensures downstream writers (which key their lookup indices on destination identifiers) can still locate the episode.

---

## Skip-Reason Logic

Before writing to a destination, providers check whether the destination is **already at or ahead of** the desired state, using `migrate.SkipReason`:

| Desired | Current destination | Skip reason |
|---|---|---|
| Played | Played | `already_played` |
| In-progress | Played | `already_played` (destination is ahead) |
| In-progress | In-progress, position ≥ desired | `already_ahead` |
| Any other | Any | — (write proceeds) |

This is enforced identically in both the Overcast and Pocket Casts write paths via the shared `migrate.SkipReason` function, producing consistent `--log-file` output.

For Overcast:
- OPML-sourced entries carry live state in `currentState`/`currentPos`
- Extended-matching entries carry **last-written** state from the episode ID cache (`ws`/`wp` fields), populated on the first successful write and used for idempotency on subsequent runs

`--force-update` bypasses the skip-reason check entirely.

---

## Title Match Date Tolerance

`--title-match-tolerance` (default: 72 hours) limits how far apart two episodes' pub dates can be when matching by title. This prevents false positives between same-named episodes published years apart (e.g. yearly anniversary episodes with the same title).

The guard is applied to:
- Strategy 2 (feedtitle) in the Overcast and Apple catalog paths
- Strategy 4 (podtitle) in the Apple catalog path
- Cross-feed off-day matching in the sync engine (±1-day candidates require fuzzy title agreement)

Setting `--title-match-tolerance 0` disables the guard (legacy behaviour, accept any date combination).

---

## `--strict-feed-match`

When `--strict-feed-match` is set, only strategies that agree on the **feed URL** are used:
- Overcast: only `feeddate` (no `feedtitle` fallback)
- Apple catalog: only strategies 1 and 2 (no cross-feed `poddate`/`podtitle`)
- Overcast extended matching: per-podcast search and subscription steps are skipped

Use this when you want to be certain an episode is only written if its feed URL is unambiguous.
