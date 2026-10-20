# sheetserve

Self-hosted REST API and MCP server for Google Sheets. It is one binary.
You configure it with one YAML file, or run it in hosted mode, where people sign in with Google
and manage their own spreadsheets and keys in a web dashboard (stored in SQLite).

- Each tab gets `GET / POST / PATCH / DELETE` routes.
- You set the allowed operations for each tab, and you can hide columns.
- API keys are scoped to tabs and operations. The config stores only their SHA-256 hashes.
- `/mcp` serves an MCP endpoint. Each key gets tools for the tabs it can reach, built from the header row.
- `/openapi.json` serves an OpenAPI 3.1 spec. It lists only the tabs, ops and columns the caller's key can use.
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

## Hosted mode (Google sign-in)

If the config file is missing or lists no spreadsheets, sheetserve runs in hosted mode.
Anyone with a Google account can sign in, pick spreadsheets from their Drive, choose the ops and hidden
columns for each tab, and create API keys. A key acts through the Google account of the user who made it.
The `listen`, `cache_ttl` and `cors_origins` settings still come from the config file, if there is one.

Google Cloud setup:

1. Enable the Google Sheets API and the Google Picker API.
2. Configure the OAuth consent screen with the scopes `openid`, `email` and `.../auth/drive.file`.
   `drive.file` lets the app open only the spreadsheets a user picks, so it does not need restricted-scope verification.
   While the app is in "Testing" status, Google expires refresh tokens after 7 days and only test users can sign in.
   Publish the app for real use.
3. Create an OAuth client of type "Web application" with the redirect URI `<base URL>/auth/callback`.
4. Create an API key for the Picker, restricted to the Picker API and your site.

Environment variables:

| Variable | Value |
|---|---|
| `SHEETSERVE_BASE_URL` | Public URL, such as `https://sheets.example.com`. On Render it defaults to `RENDER_EXTERNAL_URL`. |
| `SHEETSERVE_SECRET` | 32 random bytes, base64 (`openssl rand -base64 32`). Encrypts stored refresh tokens. Keep it stable. |
| `SHEETSERVE_DB` | SQLite file path. Defaults to `sheetserve.db`. |
| `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET` | From the OAuth client. |
| `GOOGLE_PICKER_API_KEY` | The Picker API key. |
| `GOOGLE_PROJECT_NUMBER` | The Cloud project number, so picked files are granted to the app. |

```sh
SHEETSERVE_CONFIG=/nonexistent SHEETSERVE_BASE_URL=http://localhost:8080 SHEETSERVE_SECRET=... \
  GOOGLE_CLIENT_ID=... GOOGLE_CLIENT_SECRET=... GOOGLE_PICKER_API_KEY=... GOOGLE_PROJECT_NUMBER=... go run .
```

New tabs start with no ops enabled. Turn on the ops you want for each tab in the dashboard.

## API

```sh
curl -H "Authorization: Bearer $KEY" localhost:8080/v1/shop/Products
curl -H "Authorization: Bearer $KEY" "localhost:8080/v1/shop/Orders?item=mug&limit=10"
curl -g -H "Authorization: Bearer $KEY" "localhost:8080/v1/shop/Orders?qty[gte]=2&item[contains]=tea&sort=-qty"
curl -H "Authorization: Bearer $KEY" localhost:8080/v1/shop/Orders/$ID
curl -H "Authorization: Bearer $KEY" -d '{"item":"mug","qty":2}' localhost:8080/v1/shop/Orders
curl -H "Authorization: Bearer $KEY" -X PATCH -d '{"qty":3}' localhost:8080/v1/shop/Orders/$ID
curl -H "Authorization: Bearer $KEY" -X DELETE localhost:8080/v1/shop/Orders/$ID
```

Query parameters other than `limit`, `offset` and `sort` are filters. `col=value` matches the value exactly.
`col[op]=value` uses an operator: `eq`, `ne`, `gt`, `gte`, `lt`, `lte` or `contains`.
`gt`, `gte`, `lt` and `lte` compare numerically when both values are numbers, and as strings otherwise, so ISO dates work.
`contains` ignores case. A row must match every filter.

`sort=col` sorts ascending and `sort=-col` sorts descending, with the same comparison rules. Sorting happens before `limit` and `offset`.

The API returns these status codes: `401` for a missing or invalid key, `403` when the key or tab does not allow the operation,
`404` for an unknown tab or row, `400` for an unknown or hidden column, and `502` for Google API errors.
Google API error details go to the log only.

Writes use `RAW` input, so the server never runs client data as a formula. An update changes only the cells you send,
so formulas in other columns stay in place.

## Browsers (CORS)

CORS is off by default. To let a web app call the API, list its origins in the config:

```yaml
cors_origins: ["https://app.example.com"]   # or ["*"]
```

A key used in a browser is visible to anyone who loads the page. Give browser apps a read-only key scoped to the tabs they need.

## MCP

Point an MCP client at `http://host:8080/mcp` and send `Authorization: Bearer <key>`.
Each tab gets the tools `<sheet>_<tab>_list`, `_get`, `_create`, `_update`, and `_delete`, filtered by what the key can do.
`_list` takes the same filter operators and `sort` as the REST API.

## Deploy

`docker build -t sheetserve .`, or use `render.yaml` on Render.
After deploying on Render, either add `config.yaml` and `service-account.json` as Secret Files,
or fill in the hosted-mode variables. Hosted mode keeps its database on the service's disk at `/var/data`.

## Known limits

- Run one instance only. The cache, the write locks and hosted mode's per-user state live in the process.
- In hosted mode, two users who add the same spreadsheet do not share write locks.
- If a user revokes the app's access in their Google account, their keys return `502` until they sign in again.
- An update or delete finds its row by id just before it writes. If someone inserts or deletes rows by hand in the sheet at that moment, the write can land on the wrong row.
- Filters read the whole tab. Very large tabs will be slow until the cache has them.
