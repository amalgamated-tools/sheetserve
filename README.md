# sheetserve

Self-hosted REST API and MCP server for Google Sheets. It is one binary with no database.
You configure it with one YAML file.

- Each tab gets `GET / POST / PATCH / DELETE` routes.
- You set the allowed operations for each tab, and you can hide columns.
- API keys are scoped to tabs and operations. The config stores only their SHA-256 hashes.
- `/mcp` serves an MCP endpoint. Each key gets tools for the tabs it can reach, built from the header row.
- A per-tab cache stores reads. Any write to a tab clears that tab's cache.
- Every request is logged as JSON, with the key name and the reason for any refusal.

## Setup

1. Create a Google Cloud service account, enable the Sheets API, and download its JSON key.
2. Share each spreadsheet with the service account's email address.
   Give it Viewer access for read-only use, or Editor access for writes.
3. Make sure row 1 of each tab is a header row with an `id` column.
   The server generates ids for the rows it creates.
4. Create the config and a key:

   ```sh
   cp config.example.yaml config.yaml
   go run . keygen        # paste the hash into config.yaml and give the key to the client
   GOOGLE_APPLICATION_CREDENTIALS=service-account.json go run .
   ```

## API

```sh
curl -H "Authorization: Bearer $KEY" localhost:8080/v1/shop/Products
curl -H "Authorization: Bearer $KEY" "localhost:8080/v1/shop/Orders?item=mug&limit=10"
curl -H "Authorization: Bearer $KEY" localhost:8080/v1/shop/Orders/$ID
curl -H "Authorization: Bearer $KEY" -d '{"item":"mug","qty":2}' localhost:8080/v1/shop/Orders
curl -H "Authorization: Bearer $KEY" -X PATCH -d '{"qty":3}' localhost:8080/v1/shop/Orders/$ID
curl -H "Authorization: Bearer $KEY" -X DELETE localhost:8080/v1/shop/Orders/$ID
```

Query parameters other than `limit` and `offset` are filters. A row matches only when the column value is exactly equal.

The API returns these status codes: `401` for a missing or invalid key, `403` when the key or tab does not allow the operation,
`404` for an unknown tab or row, `400` for an unknown or hidden column, and `502` for Google API errors.
Google API error details go to the log only.

Writes use `RAW` input, so the server never runs client data as a formula. An update changes only the cells you send,
so formulas in other columns stay in place.

## MCP

Point an MCP client at `http://host:8080/mcp` and send `Authorization: Bearer <key>`.
Each tab gets the tools `<sheet>_<tab>_list`, `_get`, `_create`, `_update`, and `_delete`, filtered by what the key can do.

## Deploy

`docker build -t sheetserve .`, or use `render.yaml` on Render.
After deploying on Render, add `config.yaml` and `service-account.json` as Secret Files.

## Known limits

- Run one instance only. The cache and the write locks live in the process.
- An update or delete finds its row by id just before it writes. If someone inserts or deletes rows by hand in the sheet at that moment, the write can land on the wrong row.
- Filters read the whole tab. Very large tabs will be slow until the cache has them.
