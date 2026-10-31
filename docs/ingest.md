---
layout: page
title: File Dataset Ingestion
hide_hero: true
show_sidebar: false
menubar: docs-menu
---

## Custom File Dataset Ingestion

`POST /v2/ingest/{fileType}` uploads a CSV or Senzing file that Watchman parses with a schema you define in YAML. The file type must already exist in config; an unknown name does not create a new list.

Rows are stored in MySQL or PostgreSQL when you configure `Database`. Without a database, ingest lives only in that process and is gone on restart.

After each upload, Watchman loads **every** ingested row into the same in-memory search index used for OFAC and the other downloaded lists (name-token postings, ID blocks, source/type partitions). A per-source checksum in `ingest_sources` is compared on search so Watchman does not scan the entity table again when the snapshot is unchanged. Another process sharing the database picks up a new file on the next search when the checksum changes.

`GET /v2/listinfo` includes ingested sources (row counts and checksums) next to the downloaded lists.

If you persist ingested rows in your own database as well, store [record-linkage keys](/watchman/record-linkage/) (`recordlink.Keys`) rather than raw names or identifiers.

Ingest is on the unauthenticated business API. Body cap is 32MiB by default (`413` if larger). See [Network access](/watchman/network/).

### Path Parameters

- `fileType` (required): The type of file being ingested, corresponding to a specific configuration in the Watchman YAML (e.g., `fincen-business` or `fincen-person`).

### Request Body

The body is CSV (headers matching the YAML mapping) or Senzing JSON/JSONL when `format: senzing` is set.

Oversized uploads return `413`. Override the 32MiB default with `Ingest.MaxBodyBytes` or `INGEST_MAX_BODY_BYTES`.

```yaml
  Ingest:
    Files:
      "senzing-persons":
        Format: "senzing"
```

### Response

The response is a JSON object containing the parsed entities and the file type. The structure is as follows:

```json
{
  "fileType": string,
  "entities": []searchEntity
}
```

- `fileType`: The type of file ingested (e.g., fincen-business)
- `entities`: An array of parsed entities, each containing fields like name, type, sourceID, and additional fields based on the entity type (e.g., person or business).

#### Senzing Formatting

Set the `Accept` header or `format` query parameter to receive responses in [senzing format](https://www.senzing.com/docs/entity_specification/). To receive responses as JSON Lines specify the subformat as seen below.

```
Accept: senzing       # Array of objects [{...}, {...}]

Accept: senzing/jsonl # One object per line {...}\n{...}
```

The `format` query parameter accepts this as well, `?format=senzing` or `?format=senzing/jsonl`.

### YAML Configuration

Below is an example Watchman YAML configuration for two file types: fincen-business and fincen-person.

```yaml
Watchman:
  Ingest:
    MaxBodyBytes: 33554432 # 32MiB default; oversized uploads return HTTP 413
    files:
      fincen-business:
        format: csv
        mapping:
          name:
            column: business_name
          sourceID:
            column: tracking_number
          type:
            default: "business"
          business:
            name:
              column: business_name
            altNames:
              columns: dba_name
            created:
              column: incorporated
            governmentIDs:
              type:
                column: number_type
              identifier:
                column: number
          contact:
            phoneNumbers:
              columns: phone
          addresses:
            line1:
              columns: street
            city:
              columns: city
            state:
              columns: state
            postalCode:
              columns: zip
            country:
              columns: country
      fincen-person:
        format: csv
        mapping:
          name:
            merge: [first_name, suffix, middle_name, last_name]
          sourceID:
            column: tracking_number
          type:
            default: "person"
          person:
            name:
              merge: [first_name, middle_name, last_name]
            altNames:
              merge: [alias_first_name, alias_suffix, alias_middle_name, alias_last_name]
            birthDate:
              column: dob
            governmentIDs:
              type:
                column: number_type
              identifier:
                column: number
          contact:
            phoneNumbers:
              columns: phone
          addresses:
            line1:
              columns: street
            city:
              columns: city
            state:
              columns: state
            postalCode:
              columns: zip
            country:
              columns: country
```

### Schema Explanation

- **`format`**: `csv` or `senzing` (JSON / JSONL). CSV uses `mapping` below. Senzing files skip the column mapping.
- **`mapping`**: Defines how CSV columns map to entity fields. The mapping supports:
  - **`name`**: The entity’s name, either from a single `column` or `merge` of multiple columns (e.g., combining `first_name` and `last_name`).
  - **`sourceID`**: A unique identifier for the entity, mapped to a single `column`.
  - **`type`**: The entity type (e.g., `business` or `person`), set via a `default` value.
  - **`business`** or **`person`**: Entity-specific fields, such as:
    - `name`: The primary name (can be redundant with top-level `name`).
    - `altNames`: Alternate names, mapped to a single `columns` or `merge` of multiple columns.
    - `created` (business) or `birthDate` (person): A date field, parsed from a `column` using formats like `2006-01-02`, `1/2/2006`, or `01/02/2006`.
    - `governmentIDs`: Government-issued IDs, with `type` (e.g., `tax-id`) and `identifier` mapped to columns.
  - **`contact`**: Contact information, currently supporting `phoneNumbers` mapped to a `columns` field.
  - **`addresses`**: Physical addresses, with fields like `line1`, `city`, `state`, `postalCode`, and `country`, each mapped to a `columns` field.

### CSV File Requirements

- The CSV must include headers that match the column names specified in the Watchman configuration.
- Each row represents a single entity.
- Fields like dates must conform to one of the accepted formats (`2006-01-02`, `1/2/2006`, `01/02/2006`).
- Missing or empty fields are handled gracefully (e.g., skipped or set to empty values).

### Example CSV for `fincen-business`

```csv
business_name,tracking_number,dba_name,incorporated,number_type,number,phone,street,city,state,zip,country
"Acme Corp","12345","Acme Inc","2020-01-15","tax-id","EIN123","555-1234","123 Main St","Springfield","IL","62701","US"
```

### Example CSV for `fincen-person`

```csv
first_name,middle_name,last_name,suffix,tracking_number,alias_first_name,alias_middle_name,alias_last_name,alias_suffix,dob,number_type,number,phone,street,city,state,zip,country
"John","A","Doe","Jr","67890","Johnny","B","Doe","","1985-03-22","ssn","123-45-6789","555-5678","456 Oak St","Springfield","IL","62701","US"
```

### Search the File

`fileType` is the **source**, not `type`. `type` is still the entity kind (`person`, `business`, …):

```
GET /v2/search?source=fincen-person&type=person&name=John+Doe&minMatch=0.80
```

All ingested rows of that source are in the search index. Cross-script embeddings still apply to downloaded lists; ingested files score with the same Jaro–Winkler / identifier matcher.

## rare-earth-business profile

`rare-earth-business` is a fileType shipped in `configs/config.default.yml` for entities that procure rare-earth minerals and are designated under the updated US Government rare-earth procurement regulations. It is screened during bank processing with `source=rare-earth-business`. No Go code is involved: the profile is YAML plus a CSV.

### Mapping

```yaml
Watchman:
  Ingest:
    files:
      rare-earth-business:
        format: csv
        mapping:
          name:
            column: entity_name
          sourceID:
            column: record_id
          type:
            default: "business"
          business:
            name:
              column: entity_name
            altNames:
              columns: alt_name
            governmentIDs:
              type:
                column: id_type
              identifier:
                column: id_number
          addresses:
            line1:
              columns: street
            city:
              columns: city
            state:
              columns: state
            postalCode:
              columns: zip
            country:
              columns: country
```

`type` stays `business` (entity kind). `rare-earth-business` is the **source** name used in searches and in `/v2/listinfo`.

### CSV schema

Seed file: `configs/ingest/rare-earth-entities.csv` (fictitious placeholder rows; replace with the curated list, keeping these headers).

| Header | Required | Indexed | Notes |
| ------ | -------- | ------- | ----- |
| `record_id` | yes | as `sourceID` | Stable per entity (e.g. `RE-2026-0001`). Re-uploads replace by this key, so never renumber. Repeat a `record_id` on an extra row with only `entity_name` + `alt_name` to add more aliases; rows with the same id are merged. |
| `entity_name` | yes | name | Legal / primary name. |
| `alt_name` | no | alt name | One alias per row. |
| `id_type` | no | no | Informational for the curator (`EIN`, `USCC`, `LEI`, `REG`, …). The CSV reader stores the identifier only. |
| `id_number` | no | identifier | Exact-match identifier. |
| `street`, `city`, `state`, `zip`, `country` | no | address | `country` as ISO 3166-1 alpha-2. One address per row. |
| `program`, `listed_on` | no | no | Provenance columns; unmapped headers are ignored by Watchman. |

```csv
record_id,entity_name,alt_name,id_type,id_number,street,city,state,zip,country,program,listed_on
RE-2026-0001,Meridian Lanthanide Trading Co.,Meridian Lanthanide Trading Company,EIN,84-7719024,1200 Harbor Freight Rd,Long Beach,CA,90802,US,RARE-EARTH-PROC,2026-09-15
RE-2026-0003,Baiyun Neodymium Sourcing Ltd.,Baiyun Nd Sourcing,USCC,91150200MA3DK9QX0F,88 Gangtie Dadao,Baotou,Inner Mongolia,014010,CN,RARE-EARTH-PROC,2026-09-15
RE-2026-0003,Baiyun Neodymium Sourcing Ltd.,Baiyun Neodymium Supply Chain Co.,,,,,,,,RARE-EARTH-PROC,2026-09-15
```

Confirm the exact headers with whoever curates the list before changing the mapping; the headers in the file and the `column:`/`columns:` values in YAML must match exactly.

### Persistence

Without `Database`, the rows live only in the process that received the upload and are gone on restart. For bank processing, configure MySQL or PostgreSQL. `configs/config.postgres.yml` is a ready override that matches `docker-compose.yml`:

```
docker compose up -d postgres
APP_CONFIG=configs/config.postgres.yml ./bin/server
```

With a database, every Watchman process sharing it loads `rare-earth-business` into its search index at startup, and a changed per-source checksum in `ingest_sources` is picked up on the next search by the other processes.

### Upload, verify, screen

```
# upload (re-run this whenever the regulation / list changes)
curl -sS -X POST --data-binary @configs/ingest/rare-earth-entities.csv \
  "http://localhost:8084/v2/ingest/rare-earth-business" | jq '.fileType, (.entities | length)'

# the source appears next to the downloaded lists with its row count and checksum
curl -sS "http://localhost:8084/v2/listinfo" | jq '.lists["rare-earth-business"], .listHashes["rare-earth-business"]'

# screen a counterparty; debug=true shows which fields drove the score
curl -sS "http://localhost:8084/v2/search?source=rare-earth-business&type=business&name=Meridian+Lanthanide+Trading&minMatch=0.80&debug=true" | jq .
```

`scripts/ingest-rare-earth.sh [host] [csv]` runs the upload and the `/v2/listinfo` check.

Re-upload procedure when the list changes:

1. Update the CSV. Keep existing `record_id`s; add new ids for new designations; drop rows for delistings.
2. `POST /v2/ingest/rare-earth-business` with the full file. An upload **replaces** the whole source (it is not a delta), so always send the complete list.
3. Check `/v2/listinfo`: the row count matches the file and the `rare-earth-business` hash changed. Record that hash with the change ticket.
4. Screen one added and one removed entity to confirm.

The body cap is 32MiB (`Watchman.Ingest.MaxBodyBytes` / `INGEST_MAX_BODY_BYTES`). The endpoint is on the unauthenticated business API, so restrict network access to it (see [Network access](/watchman/network/)).

### Screening guidance for bank processing

- Always send `type=business` with `source=rare-earth-business`. A search with the wrong `type` only looks in that partition and returns nothing. If a person fileType is added later, screen it with `type=person`.
- Keep `minMatch=0.80` as the production threshold.
- Send the `address` you have. A name-only query is capped near 0.80 by the coverage penalty (the index rows carry an address and an identifier the query did not), so a close-but-not-exact name such as `Volga Scandium Trade` for `Volga Scandium Trade OOO` can land just under the threshold; the same query with the counterparty's address scores higher. Use `debug=true` to see the pieces.
- `gov_*` query parameters do not boost matches against this profile today: the CSV reader stores the identifier without a type, and the exact-identifier matcher compares by type. Match on name + address, and keep `id_number` in the file for analysts.
- Alias rows are matched (`Kestrel REP`, `Ganzhou HREE Consolidators` hit), but a transliteration that shares no tokens with the primary name scores lower; add such aliases as their own rows and check them with `debug=true`.
- Watchman does not retain search queries. The calling bank-processing layer must log who was screened, when, the `rare-earth-business` hash from `/v2/listinfo` at that moment, the `minMatch` used, and the results. See [For compliance teams](/watchman/methodology/for-compliance/).

### Exporting Ingested Data

`GET /v2/export/{fileType}` returns the entities previously ingested for a given fileType.

- Supports the same `Accept` header and `?format=` query param as search for Senzing output:
  - `?format=senzing` or `Accept: senzing` → JSON array
  - `?format=senzing/jsonl` or `Accept: senzing/jsonl` → JSON Lines (NDJSON)

Example:

```
curl -s "http://localhost:8084/v2/export/fincen-person" | jq .
```

Or for Senzing format:

```
curl -s -H "Accept: senzing/jsonl" "http://localhost:8084/v2/export/fincen-person"
```

The Go client exposes this as `client.ExportFile(ctx, "fincen-person")`.

This is the read counterpart to `POST /v2/ingest/{fileType}` for backup, migration, or feeding downstream entity resolution systems.
