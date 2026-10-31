# Threat model — achilli444/finreg_demo (Moov Watchman fork)

Status: drafted by Devin from the code; the owner has not yet confirmed exposure, assets or accepted risk (see Open questions). Edit this file to refine future scan triage. No `SECURITY_MODEL.md` exists; no infrastructure repository was found, so deployment facts below are inferred from `build/Dockerfile`, `docker-compose.yml` and `configs/config.default.yml`.

## System overview
Watchman is a sanctions-screening engine (Go). It downloads OFAC/CSL/FinCEN 311/EU/UK/UN/OpenSanctions/Senzing lists, indexes them in memory, and scores customers/counterparties via an HTTP API, a Go client, a WASM web UI and an optional MCP endpoint. Optional DB (MySQL/Postgres) stores ingested custom lists and a geocoding cache. Optional outbound calls: embeddings providers (OpenAI/Ollama/Azure/OpenRouter), geocoding (Google/OpenCage/Nominatim), deepparse/libpostal sidecars.

## Components
| Component | Code |
|---|---|
| Search API | `internal/search/`, `internal/api/`, `pkg/search/` (client), `cmd/server/main.go` routing |
| Matching engine | `internal/stringscore/`, `internal/prepare/`, `internal/norm/`, `internal/index/`, `internal/indices/`, `internal/tfidf/`, `internal/embeddings/`, `internal/linksim/`, `pkg/recordlink/` |
| List ingestion | `internal/download/`, `pkg/download/`, `pkg/sources/` (parsers for OFAC, CSL, EU, UK, UN, Senzing), `internal/ingest/` (`POST /v2/ingest/{fileType}`, `GET /v2/export/{fileType}`) |
| Address & geocoding | `internal/geocoding/`, `internal/deepparse/`, `internal/postalpool/`, `cmd/postal-server/`, `pkg/address/`, `pkg/usaddress/` |
| Web UI & MCP | `internal/webui/`, `cmd/ui/`, `internal/ui/`, `internal/mcp/` |
| Admin & ops | admin server (`:9094`, `/metrics`, `/version`, `/debug/pprof`), `internal/metrics/`, `internal/config/`, `internal/db/`, `migrations/`, `build/`, `.github/workflows/` |
| Research & tooling | `research/`, `docs/`, `internal/algmatrix/`, `internal/model_validation/`, `*test` packages, benchmarks |

## Trust boundaries and entry points
- **Client → HTTP API (`:8084`)**: `GET /v2/search`, `GET /v2/listinfo`, `POST /v2/ingest/{fileType}`, `GET /v2/export/{fileType}`, `GET|POST /v2/data/refresh`, `/mcp` (if `MCP.Enabled`), web UI catch-all. **No authentication or authorization exists in code**; CORS `*` is set on ingest/export responses. TLS only if `HTTPS_CERT_FILE`/`HTTPS_KEY_FILE` set.
- **Client → admin port (`:9094`)**: metrics, version, pprof (unauthenticated unless `Pprof.Secret` set; default config comments say all profiles on).
- **Watchman → upstream list sources** (HTTPS to government/OpenSanctions URLs, overridable via `*_DOWNLOAD_URL`, `OFAC_DOWNLOAD_TEMPLATE`, Senzing `Location` incl. `file://`): parsed data crosses into the index.
- **Watchman → third-party APIs** (embeddings, geocoding) carrying query/list data and API keys (`EMBEDDINGS_API_KEY`, `GEOCODING_API_KEY`, `OPENSANCTIONS_API_KEY`).
- **Watchman → DB** (credentials in config).

## Assets
1. **Screening integrity** (assumed most critical): a missed true hit (false negative) is a sanctions violation; tampering with the index, ingested lists or refresh causes that.
2. **Screening availability**: refresh/ingest/search DoS stops onboarding/payments.
3. **Customer PII** in search queries (names, DOB, IDs, addresses) and in ingested custom lists (export endpoint returns them).
4. **Secrets**: provider API keys, DB credentials.

## Attackers
- Any network client that can reach `:8084`/`:9094` (assumed: internal network, possibly behind a gateway — unconfirmed).
- Malicious or compromised upstream list source / MITM of list downloads.
- Insider with API access.

## Existing controls
- Ingest body size cap (`INGEST_MAX_BODY_BYTES`, 32 MiB default) via `http.MaxBytesReader`.
- `api.CleanUserInput` on path vars; structured logging to avoid log injection.
- HTTP server timeouts 20s; TLS 1.2 minimum when TLS enabled.
- Refresh is single-flight (`409` when running).
- CI: zizmor workflow lint, fuzz workflow.

## Out of scope / accepted risk
- None confirmed by the user yet. Draft treats `research/`, `docs/`, benchmarks and test fixtures as low priority (not deployed).

## Open questions
- Who can reach `:8084` and `:9094`; is there an authenticating gateway?
- Which asset matters most (integrity vs PII vs availability)?
- Are unauthenticated ingest/refresh/export endpoints accepted risk?
- Is MCP enabled in production? Embeddings/geocoding providers used?
- Is there an infrastructure repository?
