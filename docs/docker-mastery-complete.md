# Docker Mastery — Complete & Extended
## From Foundations to the Bridge That Leads to Kubernetes

---

## The Mental Model That Unlocks Everything

Before a single command, the model. Docker is not a VM. A VM virtualizes the hardware — each VM gets its own kernel, its own bootloader, its own memory space managed by a hypervisor. Docker virtualizes the operating system userspace — every container shares the host kernel, but each container gets its own filesystem view, process namespace, network namespace, and resource limits. That distinction is not trivia. It explains why containers start in milliseconds (no kernel boot), why they are smaller (no guest OS), and why a Linux container cannot run natively on a Windows kernel without a Linux VM underneath it.

The three kernel features that make containers work:

**Namespaces** isolate what a process can see. There are seven namespaces Docker uses:
- `pid` — a container's PID 1 is not the host's PID 1. The container process tree is isolated.
- `net` — the container gets its own network interfaces, routing table, port space.
- `mnt` — the container gets its own mount table. The host's `/etc/passwd` is not visible unless you explicitly bind-mount it.
- `uts` — the container has its own hostname and domain name.
- `ipc` — isolated System V IPC and POSIX message queues.
- `user` — map host UIDs to container UIDs (rootless containers).
- `cgroup` — isolates the view of resource control hierarchies.

**Cgroups** (control groups) limit what a process can use. Namespaces say "you can't see outside your box." Cgroups say "you can't use more than your share." CPU shares, memory limits, disk I/O throttle, network bandwidth — all enforced by cgroups. When you pass `--memory=512m` to `docker run`, Docker creates a cgroup subtree and sets the memory limit there. The kernel OOM killer enforces it.

**Union filesystems** (OverlayFS on modern Linux) layer images. Each instruction in a Dockerfile that writes to disk creates a new layer. When a container starts, the layers are stacked: lower layers are read-only, the top layer is a read-write scratch space for that specific container. This is why two containers from the same image share all the read-only layers — they are literally the same blocks on disk, just mounted into two different namespaces. `docker images` showing 1.2GB for ten images doesn't mean 12GB of disk used; it means layers are shared.

```
MIND MAP — The Container Stack
├── Application Binary / Scripts
├── Container Layer (read-write, ephemeral, dies with container)
├── Image Layer N (read-only)
├── Image Layer N-1 (read-only)
├── ...
├── Base Image Layer (read-only, e.g. alpine:3.19)
└── Host Kernel (shared, never in the image)
```

---

## Dockerfile — Every Instruction, What It Actually Does

```dockerfile
FROM golang:1.26-alpine AS builder
```

`FROM` sets the base image. `AS builder` names this stage — you reference it later in `COPY --from=builder`. The base image is pulled from a registry (Docker Hub by default) on first use, cached locally forever after. `golang:1.26-alpine` is `golang:1.23` repackaged on Alpine Linux (~7MB) instead of Debian (~130MB). The Go toolchain size is similar either way but the OS cruft is absent. The `1.23` tag is mutable — if the maintainer pushes a new image with that tag, a fresh `docker pull golang:1.23-alpine` gets a different image. For reproducible builds, pin to a digest: `golang:1.23-alpine@sha256:abc123...`.

```dockerfile
WORKDIR /app
```

Sets the working directory inside the image for all subsequent instructions. Creates the directory if it doesn't exist. Without `WORKDIR`, every `RUN`, `COPY`, and `CMD` would execute from `/`, which is valid but confusing and risky. After this instruction, any relative path in `COPY` or `RUN` is relative to `/app`.

```dockerfile
COPY go.mod go.sum ./
RUN go mod download
```

This is the cache layer trick. You copy only the module files first, download dependencies, and only then copy source code. Why? Docker's layer cache is keyed on the instruction and the input state. If `go.mod` and `go.sum` haven't changed, `RUN go mod download` hits cache even if you changed `main.go`. If you wrote `COPY . .` followed by `go mod download`, every source file change would invalidate the download cache. For a project with 50 dependencies, that's the difference between a 2-second build and a 45-second build in CI.

```dockerfile
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /app/bin/server ./cmd/api
```

`CGO_ENABLED=0` disables C bindings, producing a pure Go binary with no libc dependency. `GOOS=linux` is explicit cross-compilation target. `-ldflags="-s -w"` strips the symbol table (`-s`) and DWARF debug information (`-w`), reducing binary size by 30–50% with zero runtime effect. The output lands at `/app/bin/server`. This is in the builder stage — it will not appear in the final image unless you copy it.

```dockerfile
FROM scratch
COPY --from=builder /app/bin/server /server
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
ENTRYPOINT ["/server"]
```

`FROM scratch` is the empty image. It has no shell, no package manager, no OS binaries. The final image contains exactly two files: your binary and the CA bundle. Every layer that existed in the builder stage is gone. `COPY --from=builder` crosses the stage boundary — it reaches into the named stage's filesystem and copies files without carrying any of the builder's layers. The CA bundle is necessary if your binary makes HTTPS calls — without it, TLS certificate verification fails with "x509: certificate signed by unknown authority." The final image is typically 7–15MB.

```
MIND MAP — Dockerfile Instruction Cache Invalidation Order
├── FROM (almost never changes)
├── ARG before FROM (build-time vars — bust cautiously)
├── WORKDIR (changes rarely)
├── COPY go.mod go.sum → RUN go mod download (bust on dep changes only)
├── COPY . . (bust on any source change)
├── RUN go build (always follows source copy)
└── FROM scratch + COPY --from=builder (fresh layer set, no contamination)
```

---

## Image Layers — The Physics of Docker Storage

Run `docker history recallo-api:latest` on any image. You will see each layer's ID, creation command, and size. Layers are content-addressed — the ID is a SHA256 of the layer's contents. Two images with identical layers share those layers on disk via OverlayFS hardlinks.

The implication: **order of instructions in a Dockerfile is a performance contract.** Least-frequently-changing instructions go first. Most-frequently-changing go last. An instruction that changes invalidates every layer below it. If you put `COPY . .` before `RUN go mod download`, your CI job spends 45 seconds downloading dependencies on every single push.

`docker image prune` removes dangling images — images with no tag that are no longer referenced by any container. These accumulate during development as you rebuild with the same tag. `docker image prune -a` removes all images not currently referenced by a running or stopped container. Do this on CI runners that have ephemeral storage. Do not do it on dev machines carelessly — you will re-pull everything on the next run.

---

## The `.dockerignore` File — Mandatory, Not Optional

When Docker runs a `COPY . .` instruction, the build context (the entire directory tree you pass to `docker build`) gets tar'd and sent to the Docker daemon. On a project with a `node_modules` directory, a `.git` folder, and local build artifacts, that context can be gigabytes. The `.dockerignore` file tells Docker to exclude paths from the context before sending.

```
.git
.github
bin/
*.md
config/dev.env
*_test.go
```

This is not about what ends up in the image — `.dockerignore` affects what is even available to the build process. A `COPY . .` with a 500MB context on every build, versus a 2MB context when you ignore everything irrelevant, is a build time regression in disguise. Check your context size by watching the "Sending build context" line: `Sending build context to Docker daemon  2.048kB` is correct. `Sending build context to Docker daemon  412.8MB` means your `.dockerignore` is wrong or missing.

---

## ARG vs ENV — Build-Time vs Runtime

```dockerfile
ARG GO_VERSION=1.26
FROM golang:${GO_VERSION}-alpine AS builder

ARG BUILD_SHA
ENV BUILD_COMMIT=${BUILD_SHA}
```

`ARG` is a build-time variable. It exists only during the build — not in the running container. You pass it with `docker build --build-arg GO_VERSION=1.22 .`. An `ARG` before `FROM` can parameterize the base image version. An `ARG` after `FROM` is scoped to that stage.

`ENV` is a runtime environment variable. It is baked into the image and available to processes in the container at runtime. You can override it at `docker run` time with `--env BUILD_COMMIT=abc123` or `--env-file .env`.

The critical nuance: `ARG` after `FROM` invalidates the layer cache the moment its value changes. If you have `ARG BUILD_SHA` and you pass the current git commit hash on every build, every build busts the cache at that layer and every instruction below it. Put `ARG BUILD_SHA` as late as possible — ideally only in the final stage, only for embedding the commit into the binary.

```
MIND MAP — ARG vs ENV Decision Tree
├── Needed only during build (compiler flags, version pins) → ARG
├── Needed at runtime (app config, feature flags) → ENV
├── Sensitive at runtime (secrets) → NEITHER — inject via --env-file at runtime
└── Changes every build (git SHA) → ARG, placed as late as possible to preserve cache
```

---

## Multi-Stage Builds — All the Patterns

**Pattern 1: Builder + Runtime (standard Go)**
Shown above. Builder has the full toolchain. Runtime has only the binary. Used everywhere you want a minimal final image.

**Pattern 2: Test Stage**
```dockerfile
FROM golang:1.26-alpine AS builder
WORKDIR /app
COPY . .
RUN go build -o /server ./cmd/api

FROM golang:1.26-alpine AS test
WORKDIR /app
COPY . .
RUN go test ./... -v -race

FROM scratch AS runtime
COPY --from=builder /server /server
ENTRYPOINT ["/server"]
```

You can build only the test stage with `docker build --target test .`. In CI: `docker build --target test .` fails the pipeline if tests fail. Then `docker build --target runtime .` produces the deployable image. The test stage uses the full Alpine Go image and has source code. The runtime stage has neither. Same Dockerfile, two different build targets, guaranteed the build that passes tests produces the image that ships.

**Pattern 3: Frontend + Backend in One Build**
```dockerfile
FROM node:20-alpine AS frontend-builder
WORKDIR /ui
COPY ui/package*.json ./
RUN npm ci
COPY ui/ .
RUN npm run build

FROM golang:1.26-alpine AS backend-builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=frontend-builder /ui/dist /app/static
RUN CGO_ENABLED=0 go build -o /server ./cmd/api

FROM scratch AS runtime
COPY --from=backend-builder /server /server
ENTRYPOINT ["/server"]
```

The Go binary serves the frontend's built assets embedded via `//go:embed static`. Single artifact, single container, zero coordination at deploy time. The frontend build and backend build run in separate stages — Docker can parallelize them if you run with BuildKit (`DOCKER_BUILDKIT=1`).

**Pattern 4: Development Stage with Hot Reload**
```dockerfile
FROM golang:1.26-alpine AS dev
RUN go install github.com/air-verse/air@latest
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
# Source is bind-mounted at runtime, not copied here
CMD ["air"]

FROM golang:1.26-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /server ./cmd/api
```

`docker compose` for dev mounts your local source tree into the container. `air` watches the mounted files and hot-reloads. `docker build --target dev .` builds the dev image. `docker build --target builder .` builds the production image. Same Dockerfile, different targets for different environments.

---

## Docker Networking — The Complete Model

Docker has four built-in network drivers. You need to understand all four because each maps to a real use case and each transfers to a concept you will hit in Kubernetes.

**bridge (default)** — creates the `docker0` bridge on the host. New containers created with `docker run` (without `--network`) join `docker0`. Containers on bridge can reach each other by IP but not by name unless they are on a user-defined bridge network. `docker0` traffic is NATed through iptables to reach the outside world. Do not use `docker0` for multi-container apps. Always create user-defined bridge networks.

**User-defined bridge** — `docker network create myapp`. Containers on this network can reach each other by container name, which Docker resolves via an embedded DNS server. If you run `docker run --network myapp --name redis redis:7-alpine`, any other container on `myapp` can connect to `redis:6379`. Name resolution is scoped to the network. This is what Compose creates automatically: every Compose project gets a user-defined bridge named `<project>_default`. The DNS resolution of service names is why `redis:6379` works inside a Compose setup.

**host** — `docker run --network host`. The container shares the host's network namespace entirely. No NAT, no port mapping, no isolation. Port 8080 inside the container is port 8080 on the host. You use this for maximum network performance (no NAT overhead) or when an application needs to manipulate host network interfaces. Security cost: a compromise of the container's process is a compromise of the host network. Valid use case: high-performance network tools, monitoring agents that need to see all host traffic.

**none** — `docker run --network none`. The container gets a loopback interface only. No external connectivity. Used for batch jobs that process local files, containers that communicate only via shared volumes, or security-sensitive workloads that must be provably network-isolated.

**macvlan / ipvlan** — assigns a real MAC address from the host's interface. The container appears on the local network as a first-class device. Used when you need the container to be directly routable on your LAN without NAT. Not a common pattern for web services but worth knowing.

```
MIND MAP — Which Network Driver to Use
├── Single container, needs internet → bridge (docker0, with port publish)
├── Multiple containers, same Compose project → user-defined bridge (automatic with Compose)
├── Multiple containers, cross-Compose-project → create named external network, attach both
├── Maximum throughput, monitoring agent → host
├── No network needed → none
└── Container must appear on LAN with real IP → macvlan
```

### Connecting Containers Across Compose Projects

Two Compose projects each have their own default network. Container A in project 1 cannot reach container B in project 2 by default. The pattern: create an external network, attach both Compose projects to it.

```bash
docker network create shared-infra
```

In each `docker-compose.yml`:
```yaml
networks:
  default:
    name: shared-infra
    external: true
```

Now both projects' containers are on the same network and can reach each other by container name. This is the pattern for running a shared Postgres + Redis in one Compose project and multiple app services in separate projects during development.

### Port Binding Is an iptables Rule

`-p 127.0.0.1:8080:8080` creates a DNAT (destination NAT) iptables rule: packets arriving at host `127.0.0.1:8080` get their destination rewritten to the container's IP at port 8080. The rule is in the `DOCKER` chain, which Docker inserts into the `FORWARD` chain. `docker ps` lists port bindings but the actual enforcement is iptables. When Docker removes a container, it removes those rules.

Publishing on `0.0.0.0` — which is the default if you write just `-p 8080:8080` — creates the rule on all host interfaces, including the public one. This is how people accidentally expose Redis to the internet: they run `docker run -p 6379:6379 redis` and forget that `0.0.0.0:6379` is publicly reachable if the firewall allows it. Always be explicit: `-p 127.0.0.1:6379:6379` for services that should be loopback-only.

---

## Volumes — Everything You Can Persist

Docker has three mechanisms for persisting or sharing data: named volumes, bind mounts, and tmpfs.

**Named volumes** — `docker volume create mydata` or declared in `docker-compose.yml`. Docker manages the storage location (typically `/var/lib/docker/volumes/mydata/_data`). Portable — if you move a volume to another machine, the data moves. Can be backed up with `docker run --rm -v mydata:/data -v $(pwd):/backup alpine tar czf /backup/mydata.tar.gz /data`. Survive `docker compose down`. Destroyed by `docker compose down -v`.

**Bind mounts** — `-v /absolute/host/path:/container/path`. The container sees the exact directory from the host. Changes are bidirectional and immediate. No Docker management of the data — it lives wherever you put it on the host. This is what you use for development hot-reload: bind-mount your source code directory into the container. Changes you make in your editor on the host appear immediately inside the container.

The performance trap: on Docker Desktop (Mac/Windows), bind mounts go through a file system sharing layer (gRPC-FUSE or VirtioFS) that is significantly slower than native Linux I/O. A Go project with 500 source files on a bind-mounted Mac workspace takes 3–4x longer to compile than on native Linux. The fix is to not run build steps inside containers on Mac and use the host Go toolchain directly. Bind mounts for dev hot-reload on Linux have no performance penalty.

**tmpfs** — `--tmpfs /tmp:rw,size=64m`. A RAM-backed temporary filesystem inside the container. Nothing persists after the container stops. Use for application temp files (caches, sessions) where persistence is undesirable and I/O performance matters. A Go HTTP server that writes request logs to a tmpfs mount and separately drains them to a log aggregator gets near-zero I/O latency.

```
MIND MAP — Volume Decision Tree
├── Persist data, managed by Docker, portable → Named Volume
├── Share host source code into container (dev) → Bind Mount
├── Share a specific host config file (read-only) → Bind Mount with :ro
├── App needs fast I/O temp space, no persistence → tmpfs
└── Backup: docker run --rm -v mydata:/src alpine tar czf /out/backup.tar.gz /src
```

### Volume Drivers

The default volume driver is `local`. It stores data on the Docker host's filesystem. For production multi-node scenarios (multiple VMs running containers that need to share a volume), you need a distributed volume driver: `rexray/s3fs` (S3-backed), `rexray/efs` (AWS EFS), `glusterfs`, `longhorn` (used in k3s/Kubernetes). These are relevant now for understanding why Kubernetes PersistentVolumes exist and what problem they solve: a pod that can be scheduled on any node needs a volume that is accessible from any node.

---

## Docker Compose — Advanced Patterns

### Profiles

```yaml
services:
  api:
    build: .
    profiles: ["app", "full"]

  worker:
    build: .
    command: ./server --mode=worker
    profiles: ["worker", "full"]

  redis:
    image: redis:7-alpine
    profiles: ["app", "worker", "full"]

  postgres:
    image: postgres:16-alpine
    profiles: ["app", "full"]
```

```bash
docker compose --profile app up    # starts api + redis + postgres
docker compose --profile worker up # starts worker + redis
docker compose --profile full up   # starts everything
```

Profiles let you run subsets of your stack without maintaining separate Compose files. The pattern for a microservices repo: every service has its own profile. Developers running only the auth service start only auth + its dependencies. Running everything is a flag change.

### Depends-on with Health Conditions

```yaml
services:
  api:
    build: .
    depends_on:
      postgres:
        condition: service_healthy
      redis:
        condition: service_healthy

  postgres:
    image: postgres:16-alpine
    environment:
      POSTGRES_PASSWORD: dev
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U postgres"]
      interval: 5s
      timeout: 3s
      retries: 10
      start_period: 10s

  redis:
    image: redis:7-alpine
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 5s
      timeout: 3s
      retries: 5
```

Without `condition: service_healthy`, `depends_on` only ensures the container is started, not that the service inside it is ready. Postgres starts in about 3 seconds but takes another 2 seconds to accept connections. An API that starts before Postgres is ready gets connection refused on startup. `service_healthy` waits for the healthcheck to pass. `start_period` gives Postgres a grace period during initialization before healthcheck failures count as failures.

### Override Files

```
docker-compose.yml          # base, always applied
docker-compose.override.yml # local dev overrides, applied automatically
docker-compose.prod.yml     # production overrides, applied explicitly
docker-compose.test.yml     # test overrides
```

```bash
docker compose up                                    # base + override (dev)
docker compose -f docker-compose.yml \
               -f docker-compose.prod.yml up         # base + prod
docker compose -f docker-compose.yml \
               -f docker-compose.test.yml up         # base + test
```

`docker-compose.override.yml` is applied automatically without specifying it. This is the correct pattern for dev-specific settings (bind mounts, debug ports, hot-reload commands) that should not exist in the base file or in the production override. The CI test run uses `-f` to be explicit — it never picks up local overrides.

```yaml
# docker-compose.override.yml (dev only)
services:
  api:
    volumes:
      - .:/app                    # bind-mount source for hot-reload
      - /app/bin                  # anonymous volume to hide the bind-mounted bin/
    environment:
      DEBUG: "true"
    ports:
      - "127.0.0.1:6060:6060"    # pprof
```

The anonymous volume trick (`/app/bin`) is worth understanding: if you bind-mount `.` into `/app`, and `/app/bin` inside the container has the built binary, the bind mount would hide `/app/bin` with your host's `bin/` directory (which may have a different-architecture binary). An anonymous volume at `/app/bin` takes precedence over the bind mount for that specific path, so the container's compiled binary is preserved while the source code on the host is live-synced.

### Extending Services with YAML Anchors

```yaml
services:
  base-api: &base-api
    build: .
    env_file: .env
    depends_on:
      redis:
        condition: service_healthy

  api:
    <<: *base-api
    command: ./server --mode=api

  worker:
    <<: *base-api
    command: ./server --mode=worker
    deploy:
      replicas: 2
```

YAML anchors (`&base-api`) and aliases (`<<: *base-api`) let you avoid repeating configuration. The `<<:` merge key merges the map. This is Docker Compose's DRY mechanism. Use it for any service that shares a base configuration (same image, same env, same dependencies) but differs in command or resource limits.

---

## Docker Build — Advanced Flags and BuildKit

BuildKit is Docker's modern build backend, enabled by default since Docker 23. With BuildKit, multi-stage builds that have no dependency between stages are built in parallel. The test stage and the frontend-builder stage from the earlier example run concurrently if they have no shared dependencies.

**Cache mounts** — mount the Go module cache as a persistent build cache:
```dockerfile
RUN --mount=type=cache,target=/root/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download

RUN --mount=type=cache,target=/root/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -o /server ./cmd/api
```

These mounts persist between builds on the same machine. The module cache and build cache are not stored as image layers — they are stored in BuildKit's cache backend. The build time drops from 45 seconds to 8 seconds after the first build because Go can reuse compiled packages. The final image contains no cache directories because the mounts are transparent — they exist only during that specific `RUN` instruction.

**Secret mounts** — inject secrets during build without baking them into layers:
```dockerfile
RUN --mount=type=secret,id=github_token \
    GITHUB_TOKEN=$(cat /run/secrets/github_token) \
    git clone https://github.com/private/repo
```

```bash
docker build --secret id=github_token,src=$HOME/.github_token .
```

The secret is available only during that `RUN` instruction, in a tmpfs at `/run/secrets/github_token`. It never appears in a layer, never in `docker history`, never in the image. Before BuildKit, people would do multi-step RUN instructions trying to scrub secrets — that does not work because each `RUN` is a layer and the layer with the secret is still there. Secret mounts are the correct approach.

**SSH agent forwarding** — for cloning private repos:
```dockerfile
RUN --mount=type=ssh \
    git clone git@github.com:your/private-repo.git
```

```bash
docker build --ssh default=$SSH_AUTH_SOCK .
```

Forwards your host's SSH agent into the build without copying private keys into the image.

**Multi-arch builds** — build for multiple architectures in a single command:
```bash
docker buildx build --platform linux/amd64,linux/arm64 \
  -t recallo/api:latest --push .
```

With Docker's `buildx` and BuildKit, you build for multiple architectures and push a multi-arch manifest to the registry. When an arm64 machine (AWS Graviton, Apple Silicon) pulls `recallo/api:latest`, it gets the arm64 binary. Same tag, correct binary, automatic. This is called a multi-arch manifest list.

```
MIND MAP — BuildKit Cache Types
├── Layer cache (default) — keyed on instruction + input state, invalidated by changes
├── --mount=type=cache — persistent, not in image, survives rebuild, ideal for pkg managers
├── --mount=type=secret — per-build, tmpfs, never in layer
├── --mount=type=ssh — SSH agent forwarding, no key in image
└── Registry cache (--cache-from, --cache-to) — share cache between CI runners via GHA cache
```

---

## Docker Registry — The Full Picture

A registry is an HTTP server that stores and serves image layers. Docker Hub is the public default. Every `docker pull golang:1.26-alpine` hits `registry-1.docker.io`.

**Self-hosted registry** — `docker run -d -p 5000:5000 --name registry registry:2`. A local registry at `localhost:5000`. Used in air-gapped environments or CI pipelines where you do not want to hit Docker Hub rate limits.

**GitHub Container Registry (ghcr.io)** — free for public repos. Authentication via `GITHUB_TOKEN` in GitHub Actions. This is the recommended registry for open-source Go projects.

**Registry rate limits** — Docker Hub limits unauthenticated pulls to 100 every 6 hours and authenticated free accounts to 200. CI runners pull images constantly. Solutions: authenticate to Docker Hub in CI, mirror base images to your own registry, or use a registry mirror on the CI runner.

**Image tagging strategy** — critical for deployment correctness:
```
recallo/api:latest          — current main branch build, mutable, never deploy by this alone
recallo/api:1.4.2           — semver tag, immutable by convention
recallo/api:abc1234         — git commit SHA, immutable, preferred for deployments
recallo/api:main-abc1234    — branch + SHA, good for multi-branch setups
```

Deploying by `latest` is deploying into uncertainty. A rollback becomes "pull whatever `latest` was before" — which is undefined. Deploying by git SHA is deterministic: `docker pull recallo/api:abc1234` gives the exact image built from that commit, today, tomorrow, and in six months.

---

## Docker Security — Not Optional

### Running as Non-Root

```dockerfile
FROM alpine:3.19
RUN addgroup -S app && adduser -S app -G app
COPY --from=builder /server /server
USER app
ENTRYPOINT ["/server"]
```

Containers run as root by default. A root process in a container that breaks out of the namespace isolation (via a kernel vulnerability) is root on the host. A non-root process that breaks out is a low-privilege user on the host. The attack surface is not zero in either case but it is categorically different in severity.

### Read-Only Root Filesystem

```yaml
services:
  api:
    security_opt:
      - no-new-privileges:true
    read_only: true
    tmpfs:
      - /tmp
```

`read_only: true` mounts the container's root filesystem read-only. The process cannot write to any path except explicitly declared writable paths. If your application is compromised and an attacker tries to write a reverse shell or modify a binary, they cannot. This forces you to be intentional about every path your application writes to — which is good hygiene.

### Capabilities

Linux capabilities break root's powers into 38 distinct capabilities. By default, Docker drops most of them but keeps several you do not need. `--cap-drop ALL` then add back only what is genuinely needed.

```yaml
services:
  api:
    cap_drop:
      - ALL
    cap_add:
      - NET_BIND_SERVICE   # only if binding to port < 1024
```

For a Go HTTP server binding to port 8080, you need no capabilities at all — `cap_drop: [ALL]` is correct with no additions.

### Seccomp Profiles

Seccomp (Secure Computing Mode) is a kernel mechanism to allow-list or deny-list system calls. Docker ships with a default seccomp profile that blocks 44 dangerous syscalls (`reboot`, `kexec_load`, etc.). A Go HTTP server needs maybe 60–80 syscalls. The docker default profile allows ~300. The difference is your unexploited attack surface. In Kubernetes, this is a `SeccompProfile` in the pod's security context.

### Image Scanning

```bash
docker scout cves recallo/api:latest     # Docker's built-in CVE scanner
trivy image recallo/api:latest           # Trivy (Aqua Security, widely used in k8s)
grype recallo/api:latest                 # Grype (Anchore)
```

An image's attack surface is the union of all software in its layers. `FROM scratch` + a static Go binary has essentially zero attack surface because there is no system software to have vulnerabilities in. `FROM ubuntu:22.04` has 300+ packages, each of which has a CVE history. Run `trivy image` in your CI pipeline. Fail the build if critical CVEs are found.

```
MIND MAP — Container Security Layers
├── Run as non-root user (UID != 0)
├── Read-only root filesystem
├── Drop ALL capabilities, add back only what's needed
├── no-new-privileges security option
├── Seccomp profile (syscall allowlist)
├── Distroless/scratch base (minimal CVE surface)
├── Image scanning in CI (Trivy, Grype, Docker Scout)
├── No secrets in image layers (use --secret mount, runtime --env-file)
└── Network isolation (user-defined bridge, not host network)
```

---

## Docker Runtime — Everything About `docker run`

Most people use `docker run` with flags they copy from documentation. Every flag is a system call to a Linux kernel API. Knowing what they do makes you dangerous.

```bash
docker run \
  --name recallo-api \
  --detach \
  --network recallo \
  --env-file /opt/recallo/.env \
  --publish 127.0.0.1:8080:8080 \
  --memory 512m \
  --memory-swap 512m \
  --cpus 1.5 \
  --read-only \
  --tmpfs /tmp:rw,size=32m \
  --cap-drop ALL \
  --security-opt no-new-privileges:true \
  --restart unless-stopped \
  --health-cmd "wget -qO- http://localhost:8080/api/v1/health || exit 1" \
  --health-interval 10s \
  --health-timeout 5s \
  --health-retries 3 \
  --log-driver json-file \
  --log-opt max-size=10m \
  --log-opt max-file=3 \
  recallo/api:abc1234
```

`--memory 512m --memory-swap 512m` — sets memory limit to 512MB, then sets swap to the same value. `--memory-swap` is the total of memory + swap. Setting both equal means no swap is available. The process gets OOM-killed when it reaches 512MB. This is intentional: you want the OOM kill to happen fast and deterministically, not after thrashing swap for minutes.

`--cpus 1.5` — the container can use at most 1.5 CPU cores worth of CPU time. Implemented via CFS bandwidth control in cgroups. On a 4-core machine, 1.5 means roughly 37.5% of total CPU. For guaranteed performance, set `GOMAXPROCS` inside the container to match `--cpus` using the `automaxprocs` library — otherwise Go's scheduler creates 4 threads but cgroup throttling causes scheduling contention.

`--restart unless-stopped` — Docker's restart policy. `no` (default): never restart. `always`: restart even if manually stopped. `unless-stopped`: restart on failure, but not if explicitly stopped. `on-failure:5`: restart on non-zero exit, up to 5 times. Use `unless-stopped` for production services.

`--log-driver json-file --log-opt max-size=10m --log-opt max-file=3` — Docker's default log driver writes to `/var/lib/docker/containers/<id>/<id>-json.log`. Without size limits, a verbose application fills the disk. `max-size=10m` rotates after 10MB. `max-file=3` keeps 3 rotated files — 30MB maximum total per container. For production, switch to `--log-driver=journald` or a centralized driver like `loki`, `fluentd`, `awslogs`.

---

## Observability — Logs, Metrics, Traces Inside Containers

### Logs

Twelve-factor: log to stdout/stderr. Docker collects stdout/stderr from the container process and routes it through the log driver. Your Go service writes structured JSON logs to stdout. Docker writes them to the json-file log. A log shipper (Filebeat, Promtail, Fluentd) reads the json-file and forwards to Elasticsearch, Loki, or CloudWatch. The container does not know or care where its logs go.

Structured logs inside a container:
```go
logger.Info("request handled",
    slog.String("method", r.Method),
    slog.String("path", r.URL.Path),
    slog.Int("status", status),
    slog.Duration("latency", duration),
)
```

The log shipper parses the JSON and indexes each field. You can then query `status > 400 AND path:/api/v1` in Grafana or Kibana. Unstructured logs give you a string that must be parsed with a regex. Structured logs are queryable by design.

### Metrics

Expose a `/metrics` endpoint from your Go service (`prometheus/client_golang`). Run a Prometheus container alongside:

```yaml
services:
  api:
    build: .
    ports:
      - "127.0.0.1:8080:8080"

  prometheus:
    image: prom/prometheus:latest
    volumes:
      - ./prometheus.yml:/etc/prometheus/prometheus.yml:ro
    ports:
      - "127.0.0.1:9090:9090"

  grafana:
    image: grafana/grafana:latest
    ports:
      - "127.0.0.1:3000:3000"
    volumes:
      - grafana_data:/var/lib/grafana
```

```yaml
# prometheus.yml
scrape_configs:
  - job_name: 'recallo-api'
    static_configs:
      - targets: ['api:8080']
    metrics_path: '/metrics'
    scrape_interval: 15s
```

`api:8080` resolves because both containers are on the same Compose network. This pattern transfers directly to Kubernetes: Prometheus becomes a `ServiceMonitor` CRD, the scrape target becomes a `Service` endpoint, and Grafana connects to the in-cluster Prometheus via a `ClusterIP` service.

### Traces

OpenTelemetry + Jaeger for distributed tracing:

```yaml
services:
  jaeger:
    image: jaegertracing/all-in-one:latest
    ports:
      - "127.0.0.1:16686:16686"  # UI
      - "127.0.0.1:4317:4317"    # OTLP gRPC
```

Your Go service sends spans to `jaeger:4317`. Jaeger collects and visualizes them at `localhost:16686`. Every HTTP request and database call becomes a span. You can see that a specific `/api/v1/transcripts` call spent 200ms in Postgres and 15ms in Redis. This is the debugging tool for latency problems that logs cannot diagnose.

---

## Docker in CI/CD — The Full Spectrum

### GitHub Actions: Build, Test, Push, Deploy

```yaml
name: CI/CD

on:
  push:
    branches: [main]

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: docker/setup-buildx-action@v3
      - name: Run tests in Docker
        run: |
          docker build --target test --cache-from type=gha --cache-to type=gha,mode=max .

  build-push:
    needs: test
    runs-on: ubuntu-latest
    outputs:
      image-digest: ${{ steps.build.outputs.digest }}
    steps:
      - uses: actions/checkout@v4
      - uses: docker/setup-buildx-action@v3
      - uses: docker/login-action@v3
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}
      - uses: docker/metadata-action@v5
        id: meta
        with:
          images: ghcr.io/${{ github.repository }}
          tags: |
            type=sha,prefix=,suffix=,format=short
            type=raw,value=latest
      - uses: docker/build-push-action@v5
        id: build
        with:
          push: true
          tags: ${{ steps.meta.outputs.tags }}
          cache-from: type=gha
          cache-to: type=gha,mode=max
          platforms: linux/amd64,linux/arm64

  deploy:
    needs: build-push
    runs-on: ubuntu-latest
    steps:
      - uses: appleboy/ssh-action@v1
        with:
          host: ${{ secrets.DROPLET_IP }}
          username: deploy
          key: ${{ secrets.SSH_PRIVATE_KEY }}
          script: |
            docker pull ghcr.io/${{ github.repository }}:${{ github.sha }}
            docker stop recallo-api || true
            docker rm recallo-api || true
            docker run -d \
              --name recallo-api \
              --restart unless-stopped \
              --env-file /opt/recallo/.env \
              --publish 127.0.0.1:8080:8080 \
              --memory 512m \
              --read-only \
              --tmpfs /tmp \
              ghcr.io/${{ github.repository }}:${{ github.sha }}
```

`type=gha` cache backend — BuildKit stores the layer cache in GitHub Actions cache storage. The build step becomes 30 seconds instead of 3 minutes after the first run. `mode=max` caches all layers, including intermediate stages.

The deploy script: `docker stop recallo-api || true` — the `|| true` prevents failure if the container does not exist (first deploy). `docker rm` removes the stopped container. `docker run` starts the new one. For zero-downtime with a single container: use `docker run` on a different port, swap Nginx upstream, then stop the old container.

---

## Image Optimization — Comprehensive Reference

```
MIND MAP — Image Size Reduction
├── Base Image Choice
│   ├── ubuntu/debian → alpine → distroless → scratch
│   ├── scratch: 0MB, only for static binaries, no shell
│   ├── distroless: 2-5MB, includes CA certs + timezone, no shell
│   └── alpine: 7MB, musl libc, busybox shell, package manager
│
├── Multi-Stage Build
│   ├── Builder stage has everything (toolchain, deps, source)
│   └── Final stage copies only the artifact
│
├── Layer Optimization
│   ├── Combine RUN commands with && to reduce layers
│   ├── RUN apt-get update && apt-get install -y pkg && rm -rf /var/lib/apt/lists/*
│   ├── Sorted package lists prevent diff churn
│   └── Clean up in the same RUN layer (separate cleanup layer is too late)
│
├── Build Flags (Go)
│   ├── CGO_ENABLED=0 → no libc dependency
│   ├── -ldflags="-s -w" → strip debug symbols (30-50% size reduction)
│   └── -trimpath → remove local build paths from binary (reproducibility)
│
├── .dockerignore
│   └── Exclude: .git, node_modules, *.md, test fixtures, local builds
│
└── BuildKit Cache Mounts
    └── Don't store pkg manager caches in layers — use --mount=type=cache
```

**Measuring**: `docker image inspect recallo/api:latest --format='{{.Size}}'` gives bytes. `dive recallo/api:latest` (the `dive` tool) shows a layer-by-layer breakdown of what is in each layer and what is wasted. A "wasted" layer is one that adds a file then deletes it in a later layer — the file is gone from the final image but its data still exists in the lower layer, contributing to image size.

---

## Container Orchestration — The Transition Point

You have mastered Docker at the single-host level. The moment your system needs to run on more than one machine, Docker alone is not enough. You need something that answers these questions automatically:

- Which machine should this container run on? (Scheduling)
- What happens if that machine goes down? (Rescheduling/self-healing)
- How do I update all running instances of a container to a new image? (Rolling updates)
- How do containers on different machines find each other? (Service discovery)
- How do I distribute traffic across multiple instances? (Load balancing)
- How do I manage secrets across a cluster? (Secret management)
- How do I scale a service up or down based on CPU usage? (Autoscaling)

Docker Swarm answers these questions with minimal configuration. Kubernetes answers them with maximum flexibility. The modern answer is Kubernetes.

---

## Prerequisites Before Kubernetes — The Honest Checklist

Kubernetes will not teach you Linux or networking. You are expected to know them. Here is what you must be solid on before you touch a kubeconfig:

**Linux fundamentals (must know)**
- Process management: signals (`SIGTERM`, `SIGKILL`), `systemd` units, process trees, `/proc`
- Networking: `ip addr`, `ip route`, `iptables -L`, DNS resolution (`/etc/resolv.conf`, `dig`, `nslookup`), TCP handshake, how a packet moves through a Linux machine
- File permissions: UID/GID, `chmod`, `chown`, `chroot`, Linux capabilities
- Systemd: `systemctl status`, `journalctl`, `ExecStart`, service dependencies — you already have this from Recallo
- Storage: mount namespaces, `df -h`, `lsblk`, filesystem types

**Networking concepts (must know)**
- OSI model layers 3 (IP routing), 4 (TCP/UDP), 7 (HTTP/HTTPS/WebSocket)
- Subnetting: CIDR notation (`10.0.0.0/24`, how many hosts, what the mask means)
- DNS: A records, CNAME, TTL, how `dig api.recallo.com` resolves
- NAT, DNAT, SNAT — you already understand this from Docker port binding
- Load balancers: Layer 4 (TCP) vs Layer 7 (HTTP) — what they see, what they can do
- TLS: Certificate chain, SNI, how HTTPS works, what a CA does

**Docker (the depth covered in this document)**
- Not just `docker run` and `docker compose up` — namespaces, cgroups, OverlayFS
- Multi-stage builds, BuildKit, registry operations
- Networking drivers, volume types, security options
- The mental model: containers are kernel namespaces + cgroups + OverlayFS

**Go-specific (already have from Recallo)**
- Graceful shutdown with signal handling — directly maps to pod termination grace period
- Health check endpoints — directly maps to readinessProbe/livenessProbe
- Structured logging to stdout — directly maps to pod log collection
- Environment-variable-driven config — directly maps to ConfigMap/Secret injection
- Connection retry with exponential backoff — directly maps to pod startup dependencies

**Systems thinking (must have)**
- Understand what a load balancer does at the TCP level
- Understand what happens when a process crashes: who restarts it, how long it takes, what requests were in flight
- Understand what "stateless service" means and what state actually goes (Redis, Postgres)
- Understand the difference between "service is running" and "service is ready"

```
MIND MAP — Kubernetes Concept <-> Docker Concept Mapping
├── Pod <-> Container (one or more containers sharing a network namespace)
├── Deployment <-> docker run --restart=always (desired state + self-healing)
├── Service (ClusterIP) <-> user-defined bridge network (internal DNS)
├── Service (NodePort/LoadBalancer) <-> docker run -p (external access)
├── Ingress <-> Nginx (TLS termination, path routing)
├── ConfigMap <-> docker run --env / --env-file (configuration injection)
├── Secret <-> docker run --env-file (sensitive config, encrypted at rest)
├── PersistentVolume <-> docker volume (storage that outlives a container)
├── PersistentVolumeClaim <-> docker run -v mydata:/data (request for storage)
├── Namespace <-> Docker Compose project (isolation boundary)
├── DaemonSet <-> docker run on every node (node-level agents, monitoring)
├── Job <-> docker run --rm (one-off task, succeeds or fails)
├── CronJob <-> cron + docker run (scheduled task)
├── HorizontalPodAutoscaler <-> docker service scale (scale on metrics)
├── ResourceRequest/Limit <-> --cpus / --memory (cgroup enforcement)
├── readinessProbe <-> depends_on healthcheck (is the service actually ready?)
├── livenessProbe <-> restart policy + healthcheck (is the service still alive?)
├── startupProbe <-> start_period in healthcheck (grace period for slow starts)
└── RBAC <-> Linux file permissions (who can do what to which resources)
```

---

## The One Principle That Carries Forward

Everything you have learned — the scratch image, the layer cache, the user-defined bridge, the BuildKit secret mounts, the healthcheck conditions, the non-root user, the read-only filesystem — is an application of one principle: **declare what you need, isolate what you run, measure what happens.**

Declare: the Dockerfile declares the artifact. The Compose file declares the topology. The build args declare the parameters. Nothing is assumed about the environment.

Isolate: namespaces isolate what the process sees. Cgroups isolate what it can use. A non-root user isolates what it can do. The user-defined bridge isolates which containers can talk.

Measure: the healthcheck measures readiness. The metrics endpoint measures behavior. The structured log measures every request. The `/proc` filesystem measures everything else.

When you sit down with a Kubernetes cluster, every resource type is an extension of this principle. A `Deployment` declares the desired state of your container. A `ResourceLimit` isolates CPU and memory. A `readinessProbe` measures readiness before traffic is sent. A `HorizontalPodAutoscaler` measures CPU and adjusts replicas.

The tools change. The principle does not. Get the principle into your bones and Kubernetes is a vocabulary change, not a conceptual leap.
