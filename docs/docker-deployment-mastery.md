# Docker & Production Deployment — End to End Mastery

## The Mental Model You Must Burn In First

Before a single command, you need to understand what Docker actually *is* at the kernel level, because once you have that, every command you ever run will make sense on its own without memorizing it.

Your operating system kernel has two features that have existed since Linux 3.8 and earlier: **namespaces** and **cgroups**. Namespaces make a process think it is alone on the machine — it gets its own view of the filesystem, its own PID 1, its own network interfaces, its own hostname. Cgroups (control groups) put hard limits on what resources that isolated group of processes can actually consume — CPU shares, memory ceiling, disk I/O rate. Docker is a tool that orchestrates the Linux kernel's own isolation primitives and adds a portable filesystem format (the image) on top.

This matters for you because it means there is no virtual machine here. There is no hypervisor. A container is a process running on your actual kernel, isolated from other processes. When your Go binary runs inside a Docker container, it is literally your binary running on the host kernel, just with a filtered view of the filesystem and network. That is why containers start in milliseconds and consume almost no overhead — they are not emulating hardware.

Here is an illustrative explanation of how Docker utilizes layered filesystems. We can visualize this using the simple concept of stacking clear, transparent sheets to build a complete application image.

### Visual Guide to Docker Layers

#### 1. The Foundation: The Base Image

We start our journey with a core OS. Imagine this as a standard transparent sheet containing the basic necessary libraries (like Debian or Alpine). Currently, it's empty of our specific application.

*(Image 1: The abstract foundation. A mostly empty, light-blue sheet labeled "Base OS (Debian)". It's transparent, waiting for structure.)*

#### 2. Adding Dependencies: The Second Layer

To run our specific application, we need to add runtime environment tools, such as Python or Node.js. Instead of modifying the Base Layer (Image 1), Docker *adds* a second transparent layer right on top.

This new layer contains *only* the new files needed for the Python installation. The original Base OS layer remains pristine and unchanged underneath.

*(Image 2: Building the stack. We stack a semi-transparent orange layer, "App Dependencies (Python)," directly on top of the blue Base OS layer (from Image 1).)*

#### 3. Adding Your Code: The Application Layer

Now we must add our actual application code (the unique business logic). This is a third layer—perhaps a green one—that contains our specific `app.py` script and related configuration files.

We stack this "Application Code" layer on top of the Python dependency layer. When you look down through the stack, you see the complete application structure: **[Base OS + Python + My Code]**.

*(Image 3: The complete Image stack. We have added a third, green translucent layer, "Application Code," on top of the orange dependency layer. This creates a functional 3-layer stack.)*

#### 4. The Final Structure: Running the Container

The previous images showed how we build a static Docker **Image**. But what happens when we *run* the container?

Image 4 is crucial for efficiency and portability.

* **The Image Layers (Read-Only):** The stack we just built (Base OS + Dependencies + Code) is frozen and becomes **Read-Only**. (Notice the "LOCK" icon in the illustration). These layers can be shared across many simultaneous containers and never change. This is what makes Docker *portable*.
* **The Container Layer (Writable):** When the application runs, it needs to write temporary files, process logs, or receive uploads. Docker adds a *thin*, vibrant, **Writable Container Layer** on the very top. This is the only place where data changes.

If this container is deleted, only the temporary Writable layer (Image 4, top) is destroyed. The underlying blue, orange, and green Image layers (Image 3, preserved below) remain intact, ready to launch a new, fresh container instantly.

The portable filesystem format Docker adds on top is called a **layer**. Every instruction in a Dockerfile creates a read-only filesystem snapshot called a layer. Those layers are stacked. When a container runs, Docker adds one final writable layer on top of all the read-only layers. This stack is what you see as the container's filesystem. When a container is deleted, only the writable top layer is deleted. The read-only image layers stay on disk and are shared between every container running from the same image. This is why pulling the same base image for ten services does not waste ten times the disk — the layers are content-addressed by SHA256 hash and stored once.

The content-addressing is also what makes the build cache work. When Docker builds an image, it hashes the instruction and its inputs. If the hash matches a layer already on disk, it reuses it. This is why `COPY go.mod go.sum ./` followed by `RUN go mod download` is the canonical Go Dockerfile pattern — go.mod and go.sum change far less frequently than your source code, so the dependency download layer gets cached and skipped on every rebuild where you only changed application code. If you instead wrote `COPY . .` before `go mod download`, every source change invalidates the cache for the download step and you wait for the full download every time.

## What an Image Actually Is

An image is not a running thing. It is an immutable artifact. Think of it as a class definition — a container is an instance of that class. The image contains:

The filesystem layers stacked on top of each other. An entrypoint — the default command that runs when a container starts. Metadata — environment variables, exposed ports, labels. That is all. It has no running process, no open files, no network connections. When you run `docker run`, Docker takes the image, adds a writable layer, sets up the namespace and cgroup boundaries, and executes the entrypoint inside those boundaries.

Images are identified by a repository name and a tag: `recallo/api:abc1234`. The name before the colon is the repository. The name after is the tag. If you omit the tag, Docker assumes `latest`. You should never rely on `latest` in production because it is mutable — `latest` today and `latest` next week can be completely different images. In the Makefile target we wrote, the tag is the short git SHA (`$(shell git rev-parse --short HEAD)`). This means every image is traceable to the exact commit that produced it, and rolling back means running the image tagged with an older commit SHA.

## Reading the Recallo Dockerfile Line by Line

```dockerfile
FROM golang:1.23-alpine AS builder
```

`FROM` declares the base image — the starting filesystem layer stack. `golang:1.23-alpine` is the official Go image built on Alpine Linux. Alpine is chosen because it is 7MB compared to Debian's 120MB, and for a build stage that gets thrown away after compilation, size of the build environment matters only in how fast CI pulls it. The `AS builder` gives this stage a name so the second stage can reference it.

```dockerfile
WORKDIR /src
```

`WORKDIR` creates the directory if it does not exist and sets it as the working directory for all subsequent instructions in this stage. It is the container-equivalent of `cd /src && mkdir -p /src`. All relative paths in `COPY` and `RUN` instructions that follow are relative to this directory.

```dockerfile
COPY go.mod go.sum ./
RUN go mod download
```

This is the cache optimization described above. These two files define your dependencies. They change only when you add or remove a dependency. `go mod download` fetches all modules into the module cache inside this layer. Because these two instructions come before copying source code, the layer is cached until go.mod or go.sum changes. On a typical development cycle where you push code changes without dependency changes, this entire step is a cache hit — it takes zero seconds.

```dockerfile
COPY . .
```

Now copy all source code. This instruction will always be a cache miss on any push that includes source changes, which is expected. Every instruction after this point in the builder stage runs fresh.

```dockerfile
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -trimpath -o /bin/recallo ./cmd/api
```

This is the build command and every flag has a reason.

`CGO_ENABLED=0` tells the Go toolchain to produce a pure Go binary with no C bindings. By default Go will link against the host system's C library (libc) for certain operations like DNS resolution and OS user lookup. A CGO-enabled binary carries a runtime dependency on a specific version of libc existing on the target system. Setting this to zero removes that dependency entirely. The binary becomes fully self-contained — it links everything it needs at compile time. This is called a statically linked binary. A statically linked Go binary will run on any Linux kernel regardless of what C libraries are installed. It will even run in a `FROM scratch` container that has no filesystem at all except the binary itself.

`GOOS=linux GOARCH=amd64` cross-compiles for Linux on x86-64 even if you are building on macOS arm64. This means you can run `make build-prod` on your MacBook M-chip and get a binary that runs correctly on a DigitalOcean Droplet.

`-ldflags="-s -w"` strips two things from the final binary. `-s` strips the symbol table. `-w` strips DWARF debug information. Both are used by debuggers (delve, gdb). In production you do not attach a debugger to a running binary, so they add size and nothing else. Stripping them typically reduces binary size by 20-30%.

`-trimpath` removes all local filesystem path information from the binary. Without this, the binary embeds absolute paths like `/home/dataraj/Documents/Programming/Go/.../main.go` in stack traces and panic messages. This leaks your local directory structure and is a minor security hygiene issue. With `-trimpath`, paths in stack traces become relative to the module root, which is clean and portable.

`-o /bin/recallo` writes the output binary to `/bin/recallo` inside the builder container's filesystem. The path does not matter much — what matters is that you know exactly where it is so the second stage can copy it.

`./cmd/api` is the Go package path to build. This matches the `main` package at `cmd/api/main.go`.

```dockerfile
FROM scratch
```

`scratch` is a special reserved word in Docker. It does not reference any image on any registry. It is the empty filesystem — no shell, no coreutils, no libc, no `/etc/` directory, no `/tmp/`. Nothing. The final image will contain only what you explicitly `COPY` into it. This produces the smallest possible image and the most minimal possible attack surface. There is no shell for an attacker to drop into, no package manager to exploit, no system binaries to abuse.

```dockerfile
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
```

This is the one piece of infrastructure the Go binary needs from outside itself. When your backend makes HTTPS calls — to Deepgram's API, OpenAI's API, LiveKit Cloud — the TLS handshake requires verifying the server's certificate against a list of trusted Certificate Authorities. That list lives in `/etc/ssl/certs/ca-certificates.crt` on Alpine. Without it, every outbound HTTPS call will fail with a certificate verification error. We copy this one file from the builder stage into the scratch image. The CA bundle is about 200KB and is the only filesystem content besides the binary.

```dockerfile
COPY --from=builder /bin/recallo /recallo
```

Copy the compiled binary from the builder stage into the root of the scratch image. The binary in the final image is `/recallo`.

```dockerfile
EXPOSE 8080
ENTRYPOINT ["/recallo"]
```

`EXPOSE` is documentation. It does not actually open a port — that happens at `docker run` time with `-p`. It tells readers of the Dockerfile and tooling like Docker Compose what port the application expects to serve on. `ENTRYPOINT` in the exec form (JSON array) sets the binary to run when the container starts. Using the exec form instead of the shell form (`ENTRYPOINT /recallo`) means the process is PID 1 directly rather than a child of `/bin/sh`, which means signals from Docker are delivered directly to your Go process. This is critical for graceful shutdown — when `docker stop` sends SIGTERM, your `signal.Notify` handler in `main.go` receives it immediately, runs the 20-second drain, and exits cleanly.

The final image produced by this Dockerfile is typically 8-12MB depending on the CA bundle. The equivalent image built on `golang:1.23-alpine` without multi-stage would be 400-600MB. The multi-stage build is not a trick — it is the standard production pattern for compiled languages.

---

## Part 2 is continued in docker-deployment-mastery-2.md
