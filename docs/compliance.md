---
layout: page
title: Compliance gap report
hide_hero: true
show_sidebar: false
menubar: docs-menu
---

# Compliance gap report

This report maps the regulatory regimes a sanctions-screening engine operates under to the
code that implements (or does not implement) each expectation, states the behavior as
found, and records what was changed. It describes **implementation status**, not legal
compliance: whether a deployment meets 31 CFR Chapter V, EU/UK restrictive measures or the
BSA is a determination for the deploying institution's compliance officer and its
regulators. Watchman is a screening engine; it is not a system of record, a case-management
system or a regulatory filing system.

Status vocabulary: **implemented** (code + test in this repo), **partially implemented**,
**not implemented**, **delegated** (deliberately the integrating application's job, with the
hand-off documented here).

## Regimes in scope

| # | Regime | What it expects of a screening engine | Lists in this repo |
|---|--------|---------------------------------------|--------------------|
| 1 | OFAC, 31 CFR Chapter V (incl. 50% rule, 31 CFR 501.603/501.604 reporting) | Screen against current SDN / Non-SDN lists, treat entities 50%+ owned by blocked persons as blocked, report blocked/rejected transactions within 10 business days, keep records 5 years | `us_ofac` (SDN, `pkg/sources/ofac`), `us_non_sdn` (`pkg/sources/us_non_sdn`), `us_csl` (`pkg/sources/csl_us`) |
| 2 | EU Council consolidated restrictive measures (e.g. Reg. 269/2014 Art. 8; Reg. 2580/2001) | Screen against the EU consolidated list, apply ownership-and-control ("owned or controlled by") analysis, report to the national competent authority | `eu_csl` (`pkg/sources/csl_eu`) |
| 3 | UK OFSI consolidated list under SAMLA 2018 and the thematic/country regulations | Screen against the OFSI consolidated list, apply the UK ownership-and-control test (>50% shares/votes or de facto control), report to OFSI | `uk_csl`, `uk_sanctions_list` (`pkg/sources/csl_uk`) |
| 4 | UN Security Council consolidated list | Screen against the UN consolidated list; member-state implementation through regimes 1-3 | `un_csl` (`pkg/sources/csl_un`) |
| 5 | FinCEN / BSA (31 USC 5318; 31 CFR 1010.430, 1020.320, 1010.653) | Screen, keep records for 5 years, escalate suspicious activity (SAR within 30 days), honor Section 311 special measures | `us_fincen_311` (`pkg/sources/fincen_311`), `us_tel` (`pkg/sources/us_tel`), plus everything above |
| — | GDPR / UK GDPR (for EU/UK deployments) | Lawful basis, storage limitation, data minimisation for both list-subject and customer PII | all |

OpenSanctions (`opensanctions_*`, `internal/download/download_opensanctions.go`) and
arbitrary Senzing-format lists are supported as additional corpora; they are aggregations,
not primary legal sources.

## 1. List coverage and freshness

### Coverage (as found: implemented)

Every source the request named is wired end to end. `internal/download/download.go`
`knownDownloadableLists` is the authoritative set; each entry has a loader that downloads via
`pkg/download.Downloader.GetFiles`, parses with the source package, and feeds
`internal/index` on success.

| List name | Download | Parse / map | Loader |
|-----------|----------|-------------|--------|
| `us_ofac` | `pkg/sources/ofac/download.go` (sdn.csv, add.csv, alt.csv, sdn_comments.csv) | `pkg/sources/ofac/reader.go`, `mapper.go` | `internal/download/download_us.go` `loadOFACRecords` |
| `us_non_sdn` | `pkg/sources/us_non_sdn/download.go` | `ofac.Read` (same CSV format) | `download_us.go` `loadUSNonSDNRecords` |
| `us_csl` | `pkg/sources/csl_us/download.go` | `pkg/sources/csl_us/reader.go`, `mapper.go` | `download_us.go` `loadCSLUSRecords` |
| `us_fincen_311` | `pkg/sources/fincen_311` | `pkg/sources/fincen_311` | `internal/download/download_fincen311.go` |
| `us_tel` | `pkg/sources/us_tel` | `pkg/sources/us_tel` | `internal/download/download_us_tel.go` |
| `eu_csl` | `pkg/sources/csl_eu/download_eu.go` | `pkg/sources/csl_eu` | `internal/download/download_eu.go` |
| `uk_csl`, `uk_sanctions_list` | `pkg/sources/csl_uk/download_uk.go` | `pkg/sources/csl_uk` | `internal/download/download_uk.go` |
| `un_csl` | `pkg/sources/csl_un/download_un.go` | `pkg/sources/csl_un` | `internal/download/download_un.go` |
| `opensanctions_*` | Senzing JSON via `OpenSanctions.Lists` + API key | `pkg/sources/senzing` | `internal/download/download_opensanctions.go` |

**Gap (configuration, unchanged):** `configs/config.default.yml` ships `IncludedLists: []`.
With no lists configured the process starts, logs `no lists have been configured!` at
*warn* level, and serves an empty corpus that returns zero hits for everything. A deployment
must set `IncludedLists` or `INCLUDED_LISTS`; a health check should assert `GET /v2/listinfo`
shows every list the institution's policy requires with a non-zero count. The new
`watchman_data_list_entities{list}` gauge supports that alert.

### Refresh cadence (as found: scheduled, 12h)

- Startup: `cmd/server/download.go` `setupPeriodicRefreshing` runs a blocking refresh; any
  failure is fatal (`main.go` exits). Watchman never serves without having loaded data once.
- Scheduled: a `time.Ticker` at `Download.RefreshInterval` (`configs/config.default.yml`:
  `"12h"`, override `DATA_REFRESH_INTERVAL`; fallback 12h if unset). Refresh is **not**
  manual-only.
- Manual: `POST /v2/data/refresh` (`internal/download/api_refresh.go`), asynchronous.

OFAC, OFSI and the EU publish updates on business days, sometimes several times a day. A 12h
interval means a designation can be live for up to 12h (plus publication lag) before this
engine sees it. That is a policy choice the institution must document; many programs run
hourly or better against a cache such as `watchman-cache`. Set `DATA_REFRESH_INTERVAL=1h`
(or lower) in production and document the chosen cadence in the screening procedure.

### Failing loudly on stale or partial data

**Gap found and fixed — partial downloads were silently accepted (high severity).**
`pkg/download/client.go` `GetFiles` downloaded each file in its own goroutine, logged
`FAILURE after ... to download` on error, and then **returned the remaining files with a nil
error**. OFAC is four files (`sdn.csv`, `add.csv`, `alt.csv`, `sdn_comments.csv`); if
`alt.csv` failed, `ofac.Read` indexed the SDN list without any aliases and the refresh
reported success with a fresh `listHashes` entry. Loaders only checked `len(files) == 0`.

Change: `GetFiles` now collects every per-file failure, closes the files it did receive,
and returns `N of M files failed to download: <joined errors>`. The loader returns that
error, `RefreshAll` fails the list (or warns if the list is in `IgnoredDownloadErrors`,
which is an explicit operator opt-in), and the refresh is marked failed. Test:
`pkg/download/client_test.go` `TestClient_GetFiles_PartialFailureIsAnError`.

**Gap found and fixed — scheduled failures were visible only in logs (high severity).**
A failed scheduled refresh logged one error and kept the previous index with no indication
of *how old* that index was and no metric to alert on. `GET /v2/data/refresh` showed
`state: failed` only until the next attempt.

Change (`internal/download/refresh.go`, `cmd/server/download.go`,
`internal/download/config.go`):

- `Status` gains `lastSuccessAt`, `consecutiveFailures`, `stale` and `maxStaleness`.
  `lastSuccessAt` survives later failures so an operator can see the age of the served data.
- New `Download.MaxStaleness` (env `DATA_MAX_STALENESS`, default `2 × RefreshInterval`).
  When the last success is older than this, `stale: true` and the process logs
  `sanctions data is STALE ...` at error level on every tick in addition to the failure.
- Prometheus metrics on the admin port (`:9094/metrics`):
  `watchman_data_refresh_last_success_timestamp_seconds`,
  `watchman_data_refresh_failures_total{trigger}`,
  `watchman_data_refresh_consecutive_failures`, `watchman_data_stale`,
  `watchman_data_list_entities{list}`. Alert on `watchman_data_stale == 1` and on
  `time() - watchman_data_refresh_last_success_timestamp_seconds` exceeding policy.
- Test: `internal/download/refresh_test.go` `TestRefresher_StalenessTracking`.

Decision recorded: stale data is still **served**, not withheld. Refusing to screen would
stop payments and push institutions toward bypassing the engine; the regulatory expectation
is that staleness is *detected and escalated*, which the status field, log line and metric
now make possible. An institution that prefers fail-closed can gate traffic on
`GET /v2/data/refresh` `.stale` in its load balancer or client.

Parse failures: every reader returns parse errors (`csv.Reader` errors propagate from
`ofac.Read`, `csl_us.Read`, Senzing `ReadEntities`, etc.), which fail the list and the
refresh. `IgnoredDownloadErrors` and `ErrorOnEmptyList: false` are the two ways a list can
degrade without failing the refresh; both are explicit configuration and should be empty /
`true` respectively in a production screening deployment.

## 2. Match scoring thresholds and overrides

### Score construction (as found; documented in `docs/methodology/index.md`)

`pkg/search/similarity.go`: weighted blend of pieces — critical identifiers 50, name 35,
address 25, supporting info 15 — then multipliers read from the environment:

| Variable | Default | Effect |
|----------|--------:|--------|
| `FINAL_SCORE_LOW_COVERAGE_MULTIPLIER` | 0.95 | Few fields compared |
| `FINAL_SCORE_MIN_REQUIRED_FIELDS_MULTIPLIER` | 0.90 | A required piece missing |
| `FINAL_SCORE_NAME_ONLY_MULTIPLIER` | 0.95 | Only the name was comparable |
| `ID_CONFLICT_PENALTY_MULTIPLIER` | 0.70 | Same ID type, different value |
| `NAME_NUMERAL_CONFLICT_PENALTY_MULTIPLIER` | 0.70 | Numerals in both names disagree |

### Default threshold (gap: documented, not changed)

The engine's **effective default `minMatch` is 0.0**: `internal/search/api_search.go`
`extractSearchMinMatch` returns `0.00` when the query parameter is absent, and
`internal/mcp/mcp_search_entities.go` initialises `MinMatch: 0.0`. Results are then capped
by `limit` (default 10, maximum 100). The documented policy values are 0.80 ("screening
default", precision 0.986 / recall 0.689 on the OpenSanctions benchmark) and ~0.59 ("high
recall", precision 0.945 / recall 0.920).

A default of 0.0 is the recall-maximising choice: nothing is hidden, the top `limit`
candidates are always returned, and the caller applies the policy cutoff. The risk is the
inverse — a caller that forgets `minMatch` and treats "any result" as a hit. The default was
left at 0.0 because changing it silently alters existing callers; every integration must
pass `minMatch` explicitly, and the audit log (section 4) records the value actually used.

### Identifier conflicts vs. strong name matches (as found; override path documented)

`shouldExactOverride` is evaluated first: an exact **unique** identifier (passport, national
ID, IMO, MMSI, aircraft serial, crypto address — `isUniqueIdentityType` in
`pkg/search/similarity_exact.go`) returns 1.0 before any penalty. Otherwise, if
`hasIdentifierConflict` finds the same ID type (and country) with different values, the
blended score is multiplied by 0.70.

Consequence a reviewer must understand: a 0.95 name-only match with a conflicting national
ID scores 0.665 — **below the 0.80 screening default**. That is by design (a different
passport number is strong evidence of a different person) but it is a true-match-suppression
risk when the customer-supplied ID is wrong or stale. Override paths, now documented in
`docs/methodology/index.md`:

1. Set `ID_CONFLICT_PENALTY_MULTIPLIER=1` to disable the penalty globally (every conflict
   then surfaces for human review).
2. Run a second, name-only query (omit the identifier) when the first returns no hits above
   threshold; the conflict cannot apply if no ID is supplied.
3. Use `debug=true` to see `idConflict` in the score pieces and queue those for review.

Date of birth is **not** an identifier conflict: a differing DOB lowers the supporting-info
piece but is never multiplied by 0.70. SWIFT/BIC: there is no BIC field in the data model;
BICs identify institutions, not legal persons, and are not treated as unique identifiers.

### "When a match is 1.0" (as found: implemented and documented)

Only `UniqueIdentity` exact matches and exact crypto addresses can force 1.0
(`shouldExactOverride`, `uniqueIdentityExact`). Tax IDs, company registration numbers,
email, phone and vessel call signs raise but do not force the score. Code and
`docs/methodology/index.md` agree.

## 3. OFAC 50% rule and ownership data

### As found (gap: relationship type lost; no percentage)

`pkg/search/models.go` `Affiliation{EntityName, Type, Details}` is the only relationship
model. `pkg/sources/ofac/mapper.go` parsed `Linked To:`, `Subsidiary Of:` and
`Owned By:` / `Controlled By:` from SDN remarks but mapped **Owned/Controlled By to
`"Subsidiary Of"`**, erasing the one relationship type OFAC's 50% rule and the EU/UK
"owned or controlled" tests turn on. No source exposes an ownership percentage. EU, UK and
UN mappers emit no affiliations at all; Senzing/OpenSanctions keep only the first
relationship role per related record.

### Change

- `Affiliation.Type` now preserves `"Owned By"` and `"Controlled By"` distinctly
  (`mapper.go`, tests in `mapper_test.go`).
- New `Affiliation.OwnershipPercent` (0-100, `0` = not stated) populated when a remark
  states a stake, e.g. `Owned By: HOLDING CO (51%)`.

### Documented limitation (not implemented — cannot be, from list data alone)

OFAC's 50% rule applies to entities that are **not on any list** but are owned 50% or more,
directly or indirectly, individually or in the aggregate, by blocked persons. The EU and UK
tests additionally cover *control*. Sanctions lists do not publish the ownership graph of
unlisted companies; only a handful of SDN remarks name owners, and almost none state a
percentage. Therefore:

- Watchman **cannot flag** an unlisted counterparty as blocked under the 50% rule.
- The institution must collect beneficial-ownership data (KYC/UBO, corporate registries,
  commercial ownership datasets), screen **each owner** through Watchman, and apply the
  aggregation rule in its own case-management system.
- Where OFAC has listed an entity *because* of ownership, the `Owned By` / `Controlled By`
  affiliation now tells the analyst which blocked person drives the designation.

## 4. Auditability and record-keeping

### As found (gap: nothing logged; list version absent from responses)

`docs/config.md` stated it plainly: "Watchman does not store search queries. There is no
screening log inside the process." `SearchResponse` carried only `query` and `entities`;
a reviewer had to correlate a result with a separate `GET /v2/listinfo` call made at about
the same time. The record-keeping expectations of 31 CFR 1010.430 (5 years) and the EU/UK
"demonstrate the measures taken" standard cannot be met from the engine's own output.

### Change — list version on every response (implemented)

`pkg/search.SearchResponse.listInfo` (`pkg/search/client.go` `ListInfo`) is attached to every
HTTP `GET /v2/search` and MCP `search_entities` response:

```json
"listInfo": {
  "lists": {"us_ofac": 17842, "eu_csl": 5120},
  "listHashes": {"us_ofac": "0ab3…", "eu_csl": "9f1e…"},
  "startedAt": "2026-10-31T03:00:00Z",
  "endedAt":   "2026-10-31T03:01:12Z",
  "version":   "v0.60.0"
}
```

The hashes are of the source files actually indexed, so a result can be tied to the exact
list publication that produced it.

### Change — structured screening audit log (implemented)

`internal/audit` decorates `internal/search.Service`. With `Audit.Enabled: true` (the new
default in `configs/config.default.yml`) **every** search — HTTP or MCP, success or error —
emits one JSON event:

| Field | Content |
|-------|---------|
| `timestamp`, `requestId`, `channel` | When; caller-supplied `requestID`; `api` or `mcp` |
| `algorithm`, `minMatch`, `limit` | The policy actually applied to this request |
| `query` / `queryHash` | Normalised input entity (if `IncludeQuery`), always a SHA-256 of it |
| `listInfo` | Same object as above — list hashes and refresh timestamps |
| `resultCount`, `topMatch`, `results[]` | Each hit's `sourceList`, `sourceId`, `name`, `type`, `match`, and `details` (score pieces) when computed |
| `durationMs`, `error` | Latency; the error string if the search failed |

Sink: `Audit.Directory` writes append-only `screening-YYYY-MM-DD.jsonl` files (opened
`O_APPEND`, rotated daily, mode 0640); an empty directory writes the same JSON to the
process log for stdout → SIEM shipping. Audit write failures are logged at error level
(`AUDIT WRITE FAILED`) but never fail the search; alert on that string. Tests:
`internal/audit/audit_test.go`.

Score breakdown: `details` is only present when the search computed pieces (`debug=true`
on HTTP, `includeDetails` on MCP). Institutions that need a per-field breakdown on every
record should enable debug for production traffic or accept the final score plus list
hash as the reproducible evidence (the scorer is deterministic for a given list hash,
algorithm and environment multipliers).

What remains **delegated**: durable storage, WORM/immutability guarantees, access control
and backup of the audit files. The engine appends to local files or stdout; the institution
must ship them to retained, access-controlled storage.

## 5. Reporting hooks — integration boundary (delegated, documented)

Watchman produces candidates and evidence; it does not make the "true match" decision or
file anything. The following obligations attach to the institution's case-management
system, which should consume the audit events / search responses:

| Obligation | Trigger | Deadline | Input Watchman provides |
|------------|---------|----------|--------------------------|
| OFAC blocked-property report (31 CFR 501.603) | Analyst confirms a true match and property is blocked | 10 business days; annual report of blocked property by 30 September | Audit event (`requestId`, `listInfo.listHashes.us_ofac`, hit `sourceId` = SDN UID, `match`) |
| OFAC rejected-transaction report (31 CFR 501.604) | Transaction rejected rather than blocked | 10 business days | Same |
| SAR (31 CFR 1020.320 et seq.) | Suspicious activity identified during review | 30 calendar days from detection (60 if no suspect identified) | Audit trail of screening history for the customer (`queryHash` correlation) |
| FinCEN 311 special measures (31 CFR 1010.653 et seq.) | Hit on `us_fincen_311` | Per the specific measure | Hit `sourceList = us_fincen_311` |
| EU reporting to the national competent authority (e.g. Reg. 269/2014 Art. 8) | Funds frozen / information relevant to the measures | "Immediately" / without delay | Hit on `eu_csl` with list hash and timestamps |
| UK report to OFSI (reporting obligations in each SAMLA regulation, e.g. Russia Regs 2019 reg. 70) | Knowledge or reasonable cause to suspect a designated person | "As soon as practicable" | Hit on `uk_csl` / `uk_sanctions_list` |
| UN | Implemented through 1-3 | — | Hit on `un_csl` |

Integration contract: consume `internal/audit` events (or the `listInfo`-bearing response),
open a case keyed on `requestId`, store the analyst disposition (true/false match, blocked /
rejected / released) alongside the event, and drive the filings above from the case. No
stub code was added for filings because a stub that does not file would be more dangerous
than an explicit boundary.

## 6. Data protection (GDPR / UK GDPR)

Two PII populations exist: list subjects (public, published by the authorities, held in
memory and re-downloaded each refresh — no retention knob needed, nothing persists beyond
the process except optional `INITIAL_DATA_DIRECTORY` caches) and **customer queries**, which
until this change the engine never stored and therefore had no retention policy for.

Change: `Audit.RetentionDays` (default 1825 = 5 years, the BSA minimum) prunes
`screening-*.jsonl` files older than the window on each daily rotation; `0` disables
pruning. `Audit.IncludeQuery: false` applies data minimisation by storing only the query
hash while keeping every other field. Documented in `docs/config.md`.

Delegated: lawful basis (compliance with a legal obligation, Art. 6(1)(c)), the DPIA for a
screening system, data-subject-request handling, and the encryption/access control of the
audit files and any `Directory` volume. Note the tension an institution must resolve in its
retention schedule: the BSA minimum (5 years) is a floor; GDPR storage limitation requires
the period to be justified and finite, not that it be short.

## Summary of changes in this PR

| Area | Status | Files |
|------|--------|-------|
| Partial download = failure | implemented | `pkg/download/client.go`, `client_test.go` |
| Refresh staleness status + metrics + `MaxStaleness` | implemented | `internal/download/refresh.go`, `config.go`, `refresh_test.go`, `cmd/server/download.go` |
| `listInfo` on every search response | implemented | `pkg/search/client.go`, `internal/download/model_stats.go`, `internal/search/api_search.go`, `internal/mcp/mcp_search_entities.go` |
| Screening audit log with retention knob | implemented | `internal/audit/*`, `internal/config/config.go`, `cmd/server/main.go`, `configs/config.default.yml` |
| Owned By / Controlled By preserved; `ownershipPercent` | implemented | `pkg/search/models.go`, `pkg/sources/ofac/mapper.go`, `mapper_test.go` |
| 50% rule evaluation | not implemented — documented limitation | this document |
| Default `minMatch` | documented (0.0), unchanged | this document, `docs/methodology/index.md` |
| Regulatory filings | delegated — integration boundary documented | this document |

## Open items for the deploying institution

1. Set `IncludedLists` to the policy-required lists and alert when `watchman_data_list_entities` is 0 for any of them.
2. Set `DATA_REFRESH_INTERVAL` (≤ 1h recommended) and `DATA_MAX_STALENESS`; alert on `watchman_data_stale`.
3. Pass `minMatch` explicitly on every call; record the policy value in the screening procedure.
4. Decide on `ID_CONFLICT_PENALTY_MULTIPLIER` (keep 0.70 with a name-only fallback query, or set 1 and review every conflict).
5. Ship `screening-*.jsonl` (or stdout audit lines) to retained, access-controlled storage; set `Audit.RetentionDays` to the approved schedule.
6. Build the 50% rule / ownership-and-control step in case management using UBO data, screening each owner.
7. Wire case management to the filings in section 5.
