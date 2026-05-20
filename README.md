# PostgreSQL Backup to Cloudflare R2

Standalone Dockerized Go service that runs `pg_dump`, compresses the dump in Go, and uploads it to Cloudflare R2 through the S3-compatible API. No backup shell scripts are used.

Cloudflare references:

- [R2 S3-compatible API setup](https://developers.cloudflare.com/r2/get-started/s3/)
- [R2 S3 API compatibility](https://developers.cloudflare.com/r2/api/s3/api/)
- [R2 object lifecycle rules](https://developers.cloudflare.com/r2/buckets/object-lifecycles/)

## Files

```text
db-backup-r2/
  Dockerfile
  docker-compose.yml
  go.mod
  go.sum
  main.go
  main_test.go
  env.example
  README.md
```

## Configuration

Create a local `.env` file:

```sh
cp env.example .env
```

Fill in all required values:

```env
# Postgres
POSTGRES_HOST=
POSTGRES_PORT=5432
POSTGRES_USER=
POSTGRES_PASSWORD=
POSTGRES_DB=

# Cloudflare R2
R2_ACCOUNT_ID=
R2_ACCESS_KEY_ID=
R2_SECRET_ACCESS_KEY=
R2_BUCKET=
R2_PREFIX=postgres

# Schedule
BACKUP_CRON=0 2 * * *
TZ=Asia/Ho_Chi_Minh

# Backup
BACKUP_RETENTION_DAYS=14
BACKUP_COMPRESSION=gzip
```

Secrets are read only from environment variables or `.env`. Do not commit `.env`.
The Go app automatically loads `.env` when present. Existing environment variables take precedence over values in `.env`.

`BACKUP_COMPRESSION` supports `gzip` and `zstd`.

## R2 Permissions

Create an R2 API token in Cloudflare with Object Read & Write access scoped to the target bucket. The R2 S3 endpoint used by this service is:

```text
https://<ACCOUNT_ID>.r2.cloudflarestorage.com
```

## Retention

Retention should be enforced by a Cloudflare R2 bucket lifecycle rule, not by the backup container.

Recommended lifecycle rule:

- Prefix: `postgres/` or the value of `R2_PREFIX` plus `/`
- Action: delete objects
- Age: `14` days

Example lifecycle JSON for `wrangler r2 bucket lifecycle set`:

```json
{
  "Rules": [
    {
      "ID": "Delete PostgreSQL backups after 14 days",
      "Status": "Enabled",
      "Filter": {
        "Prefix": "postgres/"
      },
      "Expiration": {
        "Days": 14
      }
    }
  ]
}
```

`BACKUP_RETENTION_DAYS` is validated by the Go app and must be between `1` and `14`, but the app does not delete objects during normal backups. R2 Lifecycle remains active even if this container is down.

## Run

Build and start the daily scheduler service:

```sh
docker compose up -d --build
```

Run a manual one-shot backup:

```sh
docker compose run --rm backup backup
```

The manual command exits non-zero if validation, `pg_dump`, compression, or upload fails.

The container entrypoint is the Go binary:

```text
/app/db-backup-r2
```

Supported commands:

```sh
db-backup-r2 serve
db-backup-r2 backup
```

## Backup Object Path

Backups are uploaded with UTC date folders:

```text
postgres/YYYY/MM/DD/<database>-YYYYMMDDTHHMMSSZ.dump.gz
```

Example:

```text
postgres/2026/05/20/mydb-20260520T020000Z.dump.gz
```

With `BACKUP_COMPRESSION=zstd`, the extension is `.dump.zst`.

## Restore Example

Download an object from R2, then restore with `pg_restore`:

```sh
aws s3 cp "s3://$R2_BUCKET/postgres/2026/05/20/mydb-20260520T020000Z.dump.gz" ./backup.dump.gz \
  --endpoint-url "https://$R2_ACCOUNT_ID.r2.cloudflarestorage.com"

gunzip -c ./backup.dump.gz > ./backup.dump

pg_restore \
  --host "$POSTGRES_HOST" \
  --port "$POSTGRES_PORT" \
  --username "$POSTGRES_USER" \
  --dbname "$POSTGRES_DB" \
  --clean \
  --if-exists \
  ./backup.dump
```

For `zstd` backups, use `zstd -dc ./backup.dump.zst > ./backup.dump`.

## Logs

The Go app logs:

- configuration validation failures
- scheduler start and stop
- backup start
- R2 upload path
- compressed bytes uploaded
- retention policy reminder

Container logs:

```sh
docker compose logs -f backup
```
