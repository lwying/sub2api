# Sub2API Docker Image

Sub2API is an AI API Gateway Platform for distributing and managing AI product subscription API quotas.

## Quick Start

```bash
docker run -d \
  --name sub2api \
  -p 8080:8080 \
  -e DATABASE_URL="postgres://user:pass@host:5432/sub2api" \
  -e REDIS_URL="redis://host:6379" \
  weishaw/sub2api:latest
```

## Docker Compose

```yaml
version: '3.8'

services:
  sub2api:
    image: weishaw/sub2api:latest
    ports:
      - "8080:8080"
    environment:
      - DATABASE_URL=postgres://postgres:postgres@db:5432/sub2api?sslmode=disable
      - REDIS_URL=redis://redis:6379
    depends_on:
      - db
      - redis

  db:
    image: postgres:15-alpine
    environment:
      - POSTGRES_USER=postgres
      - POSTGRES_PASSWORD=postgres
      - POSTGRES_DB=sub2api
    volumes:
      - postgres_data:/var/lib/postgresql/data

  redis:
    image: redis:7-alpine
    volumes:
      - redis_data:/data

volumes:
  postgres_data:
  redis_data:
```

## Startup and Database Recovery

Sub2API runs database migrations while starting. PostgreSQL may still be
recovering briefly after a host or Docker daemon restart. The application
retries transient PostgreSQL startup and connection errors with bounded
exponential backoff, then continues startup when the database is ready.
Permanent errors such as invalid credentials, migration checksum mismatches,
SQL errors, and incompatible data fail immediately.

The Compose deployment also checks PostgreSQL readiness with both `pg_isready`
and a simple SQL query. `depends_on: condition: service_healthy` helps order a
fresh Compose start, but application-level retries are still required when
Docker restores existing containers after a host restart.

## Environment Variables

| Variable | Description | Required | Default |
|----------|-------------|----------|---------|
| `DATABASE_URL` | PostgreSQL connection string | Yes | - |
| `REDIS_URL` | Redis connection string | Yes | - |
| `PORT` | Server port | No | `8080` |
| `GIN_MODE` | Gin framework mode (`debug`/`release`) | No | `release` |
| `SUB2API_DEPLOYMENT` | Deployment marker for images that package a prebuilt binary. Set to `docker` by the published images; only `docker` is meaningful, any other value (or none) means a native install. | No | - |

## Updating a Container Deployment

The container image owns the binary at `/app/sub2api`. Replacing that file
inside a running container would not change the image, would not survive the
next container start, and would be lost on `docker compose up -d`, so the admin
API does not offer in-app update or rollback for a container deployment:

- `GET /api/v1/admin/system/check-updates` reports
  `deployment_type: "docker"` with `binary_update_supported: false`.
- `POST /api/v1/admin/system/update` and `POST /api/v1/admin/system/rollback`
  (both the local `.backup` restore and a versioned rollback) return HTTP 409
  with reason `BINARY_UPDATE_UNSUPPORTED` before downloading anything.

Updating means pulling the new image tag and recreating the container:

```bash
docker compose pull
docker compose up -d
```

Rolling *back* is the same operation with an older image tag. Treat the image
tag as the unit of versioning; the container-internal `.backup` file path is an
artifact of native (archive/systemd) installs only.

A binary built by `Dockerfile` or `deploy/Dockerfile` carries the marker at build
time, and `Dockerfile.goreleaser` declares it for the image it packages. A native
archive build has no marker, so a native install that merely runs inside an
unrelated container keeps its in-app updater.

## Supported Architectures

- `linux/amd64`
- `linux/arm64`

## Tags

- `latest` - Latest stable release
- `x.y.z` - Specific version
- `x.y` - Latest patch of minor version
- `x` - Latest minor of major version

## Links

- [GitHub Repository](https://github.com/weishaw/sub2api)
- [Documentation](https://github.com/weishaw/sub2api#readme)
