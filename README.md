# sheetserve

Self-hosted REST API and MCP server for Google Sheets. It is one binary.
People sign in with Google and manage their spreadsheets, tabs and API keys in a web dashboard, which has a Spreadsheets page and an API keys page.
The server keeps them in SQLite.

- Each tab gets `GET / POST / PATCH / DELETE` routes.
- You set the allowed operations for each tab, and a type for each column. Writes that don't match a column's type are refused.
- API keys are scoped to tabs and operations. The database stores only their SHA-256 hashes.
- `/mcp` serves an MCP endpoint. Each key gets tools for the tabs it can reach, built from the header row.
- `/openapi.json` serves an OpenAPI 3.1 spec. It lists only the tabs, ops and columns the caller's key can use.
- When `SHEETSERVE_CACHE_TTL` is set, a per-tab cache stores reads. Any write to a tab clears that tab's cache.
  Users who added the same spreadsheet share its cache, and changing settings in the dashboard keeps it.
- Every request is logged as JSON, with its status, duration, request ID and trace ID. API requests also log the key name and the reason for any refusal.
- Sign-ins and dashboard changes, such as a new key or a tab rule, are logged as `audit` records that name the user.

![The Spreadsheets page: each tab of the demo shop spreadsheet has its allowed ops and its key column, with one Save ops button for the spreadsheet](docs/images/spreadsheets.jpg)

## Setup

Google Cloud setup:

1. Enable the Google Sheets API and the Google Picker API.
2. Configure the OAuth consent screen with the scopes `openid`, `email` and `.../auth/drive.file`.
   `drive.file` lets the app open only the spreadsheets a user picks, so it does not need restricted-scope verification.
   While the app is in "Testing" status, Google expires refresh tokens after 7 days and only test users can sign in.
   Publish the app for real use. Google asks for a home page, a privacy policy and terms of service:
   use `<base URL>/`, `<base URL>/privacy` and `<base URL>/terms`. Set `SHEETSERVE_OPERATOR` and `SHEETSERVE_CONTACT` so they
   name you, and read them before you publish. If only your Google Workspace's accounts sign in, set the user type to
   "Internal" instead.
3. Create an OAuth client of type "Web application" with the redirect URI `<base URL>/auth/callback`.
4. Create an API key for the Picker, restricted to the Picker API and your site.

Environment variables:

| Variable | Value |
|---|---|
| `SHEETSERVE_BASE_URL` | Public URL, such as `https://sheets.example.com`. On Render it defaults to `RENDER_EXTERNAL_URL`. |
| `SHEETSERVE_SECRET` | 32 random bytes, base64 (`openssl rand -base64 32`). Encrypts stored refresh tokens. Keep it stable. |
| `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET` | From the OAuth client. |
| `GOOGLE_PICKER_API_KEY` | The Picker API key. |
| `GOOGLE_PROJECT_NUMBER` | The Cloud project number, so picked files are granted to the app. |
| `SHEETSERVE_DB` | Optional. SQLite file path. Defaults to `sheetserve.db`. |
| `SHEETSERVE_CACHE_TTL` | Optional. How long to cache reads, such as `60s`. Defaults to no cache. |
| `SHEETSERVE_CORS_ORIGINS` | Optional. Comma-separated browser origins allowed to call the API. See [Browsers](#browsers-cors). |
| `SHEETSERVE_SIGNUP` | Optional. `approval`, the default, makes new users wait for an admin. `open` lets anyone who signs in use the server. See [Users](#users). |
| `SHEETSERVE_OPERATOR`, `SHEETSERVE_CONTACT` | Optional. Who runs the server, such as `Acme Inc.`, and an email address or URL to reach them. The privacy policy and terms name them. |
| `SHEETSERVE_AUTH_RELAY`, `SHEETSERVE_RELAY_ORIGINS` | Optional. Sign-in for preview deploys. See [Preview deploys](#preview-deploys). |
| `PORT` | Optional. Listen port. Defaults to `8085`. |
| `OTEL_*` | Optional. The standard OpenTelemetry variables, such as `OTEL_EXPORTER_OTLP_ENDPOINT` and `OTEL_EXPORTER_OTLP_HEADERS`. Traces, metrics and logs export over OTLP. Set `OTEL_TRACES_EXPORTER`, `OTEL_METRICS_EXPORTER` or `OTEL_LOGS_EXPORTER` to `none` to turn one off. |
| `SHEETSERVE_GOOGLE_READS_PER_MINUTE`, `SHEETSERVE_GOOGLE_WRITES_PER_MINUTE` | Optional. Your Cloud project's Google Sheets API quotas. Default `300` each. See [Google quotas](#google-quotas). |
| `SHEETSERVE_GOOGLE_USER_READS_PER_MINUTE`, `SHEETSERVE_GOOGLE_USER_WRITES_PER_MINUTE` | Optional. The per-user quotas. Default `60` each. |
| `SHEETSERVE_GOOGLE_KEY_READS_PER_MINUTE`, `SHEETSERVE_GOOGLE_KEY_WRITES_PER_MINUTE` | Optional. How much of its user's quota one API key may use. Default `30` each. `0` turns any of these off. |
| `LOG_LEVEL` | Optional. `debug`, `info`, `warn` or `error`. Defaults to `info`. `debug` adds a line for each Google Sheets call and each cache hit, miss and invalidation. |

```sh
cp .env.example .env   # fill it in
mise run run           # or: go run ./cmd/server, with the variables exported
```

Sign in and add a spreadsheet from your Drive. The Columns page for it opens next (see [Columns](#columns)).
Then turn on the ops you want for each tab. New tabs start with no ops enabled.
Then create an API key on the API keys page. Row 1 of each tab is its header row.

The API keys page shows when each key was created and last used, and how many calls to Google it has left this minute.

![The API keys page: a new storefront key scoped to the demo shop's Products and Orders tabs with read and create, never used yet](docs/images/api-keys.jpg)

## Columns

Each spreadsheet has a Columns page, which the Columns button on its card opens. For each tab, it shows the columns in row 1, and for each column a type, whether it's required, and whether it is the key.
The first time, the page suggests a type for each column from the first 50 rows. Check the suggestions, then save.

| Type | A value must be |
|---|---|
| Any | Anything. A tab that was never saved on the Columns page has only Any columns. |
| Text | A JSON string |
| Number | A JSON number |
| Integer | A JSON number with no fraction |
| True/false | `true` or `false` |
| Date | A string such as `2026-01-31` |
| Date and time | An RFC 3339 string such as `2026-01-31T09:30:00Z` |
| One of | One of the values you list, such as `draft, live, retired`. Picking it fills in the values the column already holds. |

- A create or update with a value of the wrong type returns `400`. So does a create without a required column.
- A column can have a default, which a create that leaves the column out gets. A required column with a default can be left out.
  The key column can't have one. Defaults apply to hidden columns too, so a hidden `source` column can default to `api`.
- `null` empties a cell, except in a required column. `""` empties a Text or Any column only.
- Reads return each value as its column's type. A blank cell reads as `null`, and a number stored as text reads as a number.
  A cell edited by hand to a value that doesn't fit, such as `n/a` in a Number column, comes back as it is.
- The OpenAPI spec and the MCP tools give each column its type.
- Date and Date and time values are written as date cells, and read back as ISO 8601 text whatever the spreadsheet's locale.
  Times are stored in UTC, and a time typed into the sheet by hand is read as UTC.
- Filters on a typed column must fit its type, so `points[gte]=lots` returns `400`. Numbers match by value (`2` matches `2.0`),
  True/false ignores case, and `gt`, `gte`, `lt` and `lte` skip empty cells. `contains` takes any text.

Every column is in the API unless you hide it. Under Advanced, you can hide columns. A hidden column is left out of responses, the OpenAPI spec and the MCP tools, and it can't be written or filtered on.

Each tab has a key column, which identifies a row in `GET`, `PATCH` and `DELETE /v1/<sheet>/<tab>/<key>`. It is `id` unless you
pick another column, such as `email` or `sku`, on the Columns page.

- With `id` as the key, the server generates the id of each row it creates, and clients can't set or change it.
- With another key, a create must include it and its value must be new. An update can't change it.
- A key value that more than one row holds returns `409`, so the server never guesses which row you meant.
- A tab whose header has no key column can only be listed. Its other routes return `403`.

To start from nothing, create a blank spreadsheet in the dashboard instead. List its tabs like tables:
each column with a type and whether it's required, and the key column that finds a row.
The server creates the spreadsheet in your Drive with your Google account, writes and freezes each header row,
and saves the column types. A tab with no key column gets an `id` column first, unless you list one.
`drive.file` covers files the app creates, so you don't need to pick it.

![The Create a spreadsheet form: a Products tab keyed by sku with a status that is one of draft, live or retired, and an Orders tab that gets an id column, with defaults for stock, status and qty](docs/images/create-spreadsheet.jpg)

A key acts through the Google account of the user who made it.

If Google refuses that account's sign-in, because it expired or the user revoked it, every dashboard page shows a
**Reconnect Google** button and the user's keys get `503` until they sign in again.

## Users

The first person to sign in becomes the admin. By default everyone after that waits as pending until an admin approves them on
the Users page. With `SHEETSERVE_SIGNUP=open`, such as for a hosted service anyone can use, new users are members as soon as
they sign in, and an admin can still set someone to pending to suspend them.
Pending users can't add spreadsheets or make keys, and their keys return `403`.

| Role | Can |
|---|---|
| `admin` | Everything a member can, and approve users, change their roles, or remove them |
| `member` | Add spreadsheets, set tab rules, and make keys that work |
| `pending` | Nothing yet. They see a "waiting for approval" page |

Each user only sees their own spreadsheets and keys, admins included. Removing a user deletes their spreadsheets and keys.
The server always keeps at least one admin.

## API

```sh
curl -H "Authorization: Bearer $KEY" localhost:8085/v1/shop/Products
curl -H "Authorization: Bearer $KEY" "localhost:8085/v1/shop/Orders?item=mug&limit=10"
curl -g -H "Authorization: Bearer $KEY" "localhost:8085/v1/shop/Orders?qty[gte]=2&item[contains]=tea&sort=-qty"
curl -H "Authorization: Bearer $KEY" localhost:8085/v1/shop/Orders/$ID
curl -H "Authorization: Bearer $KEY" -d '{"item":"mug","qty":2}' localhost:8085/v1/shop/Orders
curl -H "Authorization: Bearer $KEY" -X PATCH -d '{"qty":3}' localhost:8085/v1/shop/Orders/$ID
curl -H "Authorization: Bearer $KEY" -X DELETE localhost:8085/v1/shop/Orders/$ID
```

Query parameters other than `limit`, `offset` and `sort` are filters. `col=value` matches the value exactly.
`col[op]=value` uses an operator: `eq`, `ne`, `gt`, `gte`, `lt`, `lte` or `contains`.
`gt`, `gte`, `lt` and `lte` compare numerically when both values are numbers, and as strings otherwise, so ISO dates work.
In a column that mixes numbers and text, numbers sort before text.
`contains` ignores case. A row must match every filter.

`sort=col` sorts ascending and `sort=-col` sorts descending, with the same comparison rules. Sorting happens before `limit` and `offset`.
A list's `X-Total-Count` header says how many rows match its filters, before `limit` and `offset`, so a client knows how many pages there are.

A successful create returns `201`. Errors use these status codes:

| Status | Cause |
|---|---|
| `400` | An unknown or hidden column, a value that doesn't match its column's type, a missing required column, an unknown filter operator, a bad `limit` or `offset`, an `id` field in a create or update, a create without the tab's key column, a change to a row's key column, or an update with no fields |
| `401` | A missing or invalid key |
| `403` | The key or the tab does not allow the operation, or the tab has no key column |
| `404` | An unknown tab or row |
| `409` | A create with a key value that exists, a key value that more than one row holds, or rows that kept moving during an update or delete |
| `429` | A Google quota is used up. `Retry-After` says when to try again. See [Google quotas](#google-quotas). |
| `502` | A Google API error |
| `503` | Google refused the key owner's sign-in: it expired or they revoked it. They must sign in to the dashboard again. |

Error bodies look like `{"error": "..."}`. Google API error details go to the log only.

Writes send every string as text, so the server never runs client data as a formula or reads it as a number or date. An update changes only the cells you send,
so formulas in other columns stay in place.

## Google quotas

The Sheets API allows each Cloud project a number of read and write requests a minute, and each Google user a smaller number.
They are under IAM & Admin, Quotas, in the Cloud Console. The server keeps every call to Google under 90% of them, so Google
doesn't refuse calls:

- One read and one write budget for the project, one for each user, and one for each API key, so a busy key can't use its
  user's whole quota.
- Only calls to Google count. A read the cache answers costs nothing.
- A call waits up to 2 seconds for room. Past that, the API returns `429` with a `Retry-After` header that says when there is
  room, and the dashboard says so.
- If Google returns `429` anyway, the call is tried twice more, after about 1 and 2 seconds.

Every REST response to a valid key says how much quota the key has left, in the `RateLimit-Policy` and `RateLimit` headers
of the [IETF draft](https://datatracker.ietf.org/doc/draft-ietf-httpapi-ratelimit-headers/):

	RateLimit-Policy: "read";q=27;w=60, "write";q=27;w=60
	RateLimit: "read";r=25;t=5, "write";r=27;t=0

`q` is how many calls the key can make a minute, `r` how many it has left over the next minute, and `t` how many seconds until
it's back to `q`. Each is from whichever budget, the key's, its user's or the project's, has the least left. At most a tenth of
`q` can go at once; spread the rest over the minute. MCP responses don't carry these headers.

The `google.limiter.waited`, `google.limiter.refused` and `google.rate_limited` metrics count each case by kind (`read` or
`write`) and budget (`project`, `user` or `key`). If you raise a quota in the Cloud Console, raise its variable too.

## Browsers (CORS)

CORS is off by default. To let a web app call the API, list its origins in `SHEETSERVE_CORS_ORIGINS`:

```sh
SHEETSERVE_CORS_ORIGINS=https://app.example.com   # or *
```

A key used in a browser is visible to anyone who loads the page. Give browser apps a read-only key scoped to the tabs they need.

## MCP

Point an MCP client at `http://host:8085/mcp` and send `Authorization: Bearer <key>`.
Each tab gets the tools `<sheet>_<tab>_list`, `_get`, `_create`, `_update`, and `_delete`, filtered by what the key can do.
`_list` takes the same filter operators, `sort`, `limit` and `offset` as the REST API.

## Deploy

`docker build -t sheetserve .`, or use `render.yaml` on Render.
After deploying on Render, fill in the `GOOGLE_*` and `SHEETSERVE_SECRET` variables. The database lives on the service's disk at `/var/data`.

### Preview deploys

Google accepts only redirect URIs that are registered in advance, with no wildcards.
A preview deploy, such as `https://sheetserve-pr-42.onrender.com`, can't sign in with its own URL.
Instead, previews sign in through the main server's relay:

1. Add `<base URL>/auth/relay` to the OAuth client's redirect URIs, next to `<base URL>/auth/callback`.
2. Set these on the main service. Render copies them to its previews.

   ```sh
   SHEETSERVE_AUTH_RELAY=https://sheetserve.onrender.com/auth/relay
   SHEETSERVE_RELAY_ORIGINS=https://sheetserve-pr-*.onrender.com
   ```

3. Leave `SHEETSERVE_BASE_URL` unset on Render, so that each preview uses its own `RENDER_EXTERNAL_URL`.
4. Add each preview's URL, such as `https://sheetserve-pr-42.onrender.com/*`, to the Picker API key's website restrictions.
   These restrictions allow a `*` only for a whole subdomain, so `sheetserve-pr-*` is not accepted.
   `https://*.onrender.com/*` works, but it lets every site on onrender.com use the key.

A server whose base URL is the relay's ignores `SHEETSERVE_AUTH_RELAY` and signs in directly.
The relay sends Google's answer only to origins that match `SHEETSERVE_RELAY_ORIGINS`.
A `*` matches any part of the host. The code it sends on is useless without the preview's PKCE verifier and the client secret.

## Development

The repo uses [mise](https://mise.jdx.dev) for its tools (Go and golangci-lint, pinned in `mise.toml`) and its tasks.

```sh
mise install          # install the pinned tools
mise tasks            # list the tasks
mise run check        # go vet, golangci-lint and the unit tests
mise run build        # build ./sheetserve
mise run run          # run the server
mise run db:new <name> # create a database migration
```

The SQLite schema lives in `internal/db/migrations` as [goose](https://github.com/pressly/goose) migrations.
They are built into the binary, and the server applies new ones when it opens the database.

mise loads `.env` into every task, if the file exists. Copy `.env.example` to `.env` and fill it in.

`mise run test:live` runs `TestLive` against a real spreadsheet. It creates a scratch tab and deletes it afterwards.
It needs `SHEETSERVE_LIVE_SHEET_ID`, set to a spreadsheet that the test's Google credentials can edit.
The test uses Application Default Credentials, such as a key file named by `GOOGLE_APPLICATION_CREDENTIALS`.
The server itself does not use them.
Without `SHEETSERVE_LIVE_SHEET_ID`, the test skips.

CI (`.github/workflows/ci.yml`) runs `mise run check` and `mise run build` on pushes to `main` and on pull requests.

## Known limits

- Run one instance only. The cache, the write locks and the per-user state live in the process.
- Users who added the same spreadsheet share its cached tabs, but each reads the cache only while their own last read from
  Google is within `SHEETSERVE_CACHE_TTL`. Someone who loses access in Google can keep reading cached rows for up to that long.
- If a user revokes the app's access in their Google account, their keys return `502` until they sign in again.
- An update or delete finds its row by key, then reads that row's key cell back just before it writes. If someone inserted or
  deleted rows by hand in between, it finds the row again, and after three tries it returns `409`. That costs one extra read
  per update or delete. Only the moment between that check and the write is unguarded.
- Filters read the whole tab. Very large tabs will be slow until the cache has them.
