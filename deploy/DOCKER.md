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

The container image owns the binary at `/app/sub2api`. The admin API reports
`deployment_type: "docker"` with `binary_update_supported: true`, so an
administrator sees the same in-app **Update Now** (立即更新) button as on a
native install: the release binary is downloaded and swapped in place.

### What that in-app update actually changes

The swap is a **writable-layer change, not an image upgrade**:

- The image tag in your compose file does not change and no new image is pulled.
- A normal restart of the same container keeps the swapped binary. That includes
  `docker restart`, `docker compose restart`, `docker start` of the existing
  container, and a restart policy (`restart: always` / `unless-stopped`) acting
  on the process exit that the in-app **Restart Now** button performs.
- **Recreating** the container discards it. `docker compose up -d` after
  changing the tag, `docker compose up -d --force-recreate`, `docker compose
  down && docker compose up -d`, `docker rm` plus `docker run`, or any other
  re-created container starts again from the image — the instance silently
  reverts to the binary the tag pins, and the in-app update is gone.

So an in-app update is never the durable record of a version. The permanent path
is always the image tag, in the compose file you maintain:

```yaml
# docker-compose.yml
services:
  sub2api:
    image: <your-fork-image>:x.y.z # pin the release you want
```

```bash
docker compose pull
docker compose up -d
```

Recreating the container from that pinned tag is the operation that makes the
version persistent and reproducible.

The updater creates temporary files and renames the executable under `/app`:
the running user needs **write access to the `/app` directory**, not merely to
`/app/sub2api`. A read-only root filesystem (`read_only: true`) or an unwritable
`/app` makes the in-app update fail; use the image-tag path above instead. The
published Dockerfiles grant the runtime user directory access. Containers without
a restart policy, or with `restart: on-failure`, remain stopped after the in-app
Restart Now action exits cleanly; start them explicitly with `docker start`.

### Rollback stays operator-managed

`POST /api/v1/admin/system/rollback` (both the local `.backup` restore and a
versioned rollback) still returns HTTP 409 with reason
`BINARY_UPDATE_UNSUPPORTED`, and the badge keeps its in-app rollback disabled
for a container deployment. The in-app update writes its own `.backup` file into
the writable layer as well, so a rollback through it would be undone by the next
container recreate. Rolling *back* is the same image operation with an older
tag:

```yaml
# docker-compose.yml
services:
  sub2api:
    image: <your-fork-image>:x.y.z-1
```

```bash
docker compose pull
docker compose up -d
```

Treat the image tag as the unit of versioning; the container-internal `.backup`
file path is an artifact of native (archive/systemd) installs only.

A binary built by `Dockerfile` or `deploy/Dockerfile` carries the marker at build
time, and `Dockerfile.goreleaser` declares it for the image it packages. A native
archive build has no marker, so a native install that merely runs inside an
unrelated container keeps its in-app updater and rollback.

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
