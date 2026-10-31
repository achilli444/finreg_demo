# Ingest seed files

CSV files uploaded to `POST /v2/ingest/{fileType}`. Column mappings live in
`configs/config.default.yml` under `Watchman.Ingest.files`.

| File | fileType (`source=`) | Entity `type` |
| ---- | -------------------- | ------------- |
| `rare-earth-entities.csv` | `rare-earth-business` | `business` |

`rare-earth-entities.csv` is a **seed / placeholder** list with fictitious
entities so the profile can be exercised end to end. Replace it with the
curated list of entities designated under the updated US Government
rare-earth procurement regulations, keeping the same headers and a stable
`record_id` per entity. See `docs/ingest.md` ("rare-earth-business profile")
for the schema, the upload command and the re-upload procedure.
