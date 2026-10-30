# Self-hosting sheetserve

sheetserve is one binary with a SQLite database. Running it takes four things:

1. A Google Cloud project that lets people sign in and pick spreadsheets.
2. A secret to encrypt the Google tokens it stores.
3. A place to run the container, with a volume for the database.
4. HTTPS in front of it.

## 1. Set up a Google Cloud project

Do this once, at [console.cloud.google.com](https://console.cloud.google.com).

1. **Create a project.** Note its **project number** on the project's Dashboard. It's all digits, such as
   `123456789012`. It is not the project ID.
2. **Turn on the APIs.** Under **APIs & Services**, then **Library**, enable the **Google Sheets API** and the
   **Google Picker API**.
3. **Set up sign-in.** Open **Google Auth Platform** (called **OAuth consent screen** in older consoles):
   - **Branding:** the app name people see, such as `sheetserve`, a support email, and your server's URLs:
     home page `https://<your host>/`, privacy policy `https://<your host>/privacy`, terms of service
     `https://<your host>/terms`. Add your domain under authorized domains.
   - **Audience:** pick **Internal** if only accounts in your Google Workspace will sign in. Internal apps need no review
     and their sign-ins don't expire. Pick **External** for anyone else.
   - **Data access:** add the scopes `openid`, `.../auth/userinfo.email` and `.../auth/drive.file`. `drive.file` lets
     sheetserve open only the spreadsheets each person picks or creates with it. It is not a sensitive scope, so Google
     doesn't make you pass a security assessment.
4. **Create the OAuth client.** Under **Clients**, create a client of type **Web application**, with the authorized
   redirect URI `https://<your host>/auth/callback`. Note its **client ID** and **client secret**.
5. **Create the Picker key.** Under **APIs & Services**, then **Credentials**, create an **API key**. Restrict it to
   the **Google Picker API**, and to websites `https://<your host>/*`.
6. **Publish (External only).** Under **Audience**, click **Publish app**. While an External app is in **Testing**,
   only the test users you list can sign in, and Google expires every sign-in after 7 days. People then see a
   **Reconnect Google** banner, and their API keys get `503` until they sign in again. Google may ask you to verify
   your domain and branding before the app is published.

## 2. Make a secret

```sh
openssl rand -base64 32
```

This key encrypts the Google refresh tokens in the database. Keep it with your other secrets and don't change it: with a
new secret, every user has to sign in again.

## 3. Run the container

Images are published to `ghcr.io/amalgamated-tools/sheetserve` for `linux/amd64` and `linux/arm64`:

- `latest` and `<version>`, such as `1.2.0`, for releases
- `main` for the newest commit

Put the settings in an env file, such as `sheetserve.env`:

```sh
SHEETSERVE_BASE_URL=https://sheets.example.com
SHEETSERVE_SECRET=<from step 2>
GOOGLE_CLIENT_ID=<from step 1.4>
GOOGLE_CLIENT_SECRET=<from step 1.4>
GOOGLE_PICKER_API_KEY=<from step 1.5>
GOOGLE_PROJECT_NUMBER=<from step 1.1>

# Who runs this server, for the privacy policy and terms
SHEETSERVE_OPERATOR=Example Inc.
SHEETSERVE_CONTACT=admin@example.com

# Optional
SHEETSERVE_CACHE_TTL=60s
# SHEETSERVE_SIGNUP=open
```

Then run it with a volume at `/data`, where the image keeps its database:

```sh
docker run -d --name sheetserve --restart unless-stopped \
  -p 8085:8085 -v sheetserve-data:/data --env-file sheetserve.env \
  ghcr.io/amalgamated-tools/sheetserve:latest
```

Or with Docker Compose:

```yaml
services:
  sheetserve:
    image: ghcr.io/amalgamated-tools/sheetserve:latest
    restart: unless-stopped
    ports: ["8085:8085"]
    env_file: sheetserve.env
    volumes: ["data:/data"]
volumes:
  data:
```

`GET /healthz` returns `200` when the server is up. The README lists every setting.

The first person to sign in becomes the admin. By default, everyone after that waits on the **Users** page until an
admin approves them. Set `SHEETSERVE_SIGNUP=open` to let anyone who signs in use the server.

### Render

`render.yaml` in this repository deploys sheetserve with a disk for the database. After the first deploy, set the
`GOOGLE_*` and `SHEETSERVE_SECRET` variables in the Render dashboard. `SHEETSERVE_BASE_URL` defaults to the service's
URL.

## 4. HTTPS

Google requires HTTPS for the redirect URI on any host but `localhost`. Put sheetserve behind a proxy that
terminates TLS, such as Caddy:

```
sheets.example.com {
	reverse_proxy localhost:8085
}
```

The session cookie is marked `Secure` whenever `SHEETSERVE_BASE_URL` starts with `https://`.

## Backups and upgrades

All of sheetserve's own state is the SQLite database in `/data`: users, sessions, spreadsheet settings and key hashes.
The spreadsheets themselves stay in Google Drive. To back it up, run SQLite's backup command on the database file in the
volume. It is safe while the server is running:

```sh
docker run --rm -v sheetserve-data:/data -v "$PWD":/backup alpine   sh -c 'apk add -q sqlite && sqlite3 /data/sheetserve.db ".backup /backup/sheetserve-backup.db"'
```

To upgrade, pull the new image and restart the container. The server applies any database migrations when it starts.
Run one instance at a time: each instance keeps its own cache and rate limits in memory.
