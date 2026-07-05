# Production Deployment Blueprint: DigitalOcean Droplet + PMGo + Vercel Integration

This guide provides the complete, end-to-end practical blueprint for running the Recallo Go backend on a DigitalOcean Droplet using **PMGo** (a PM2-like process manager for Go) or native systemd, integrated with a Vercel-deployed frontend on the root domain `recallo.io`.

---

## 1. The Global Architecture Map

The system topology maps public endpoints to internal services across different cloud environments:

```
                  ┌────────────────────────┐
                  │   DNS Registrar /      │
                  │   Cloudflare Name      │
                  │   Servers              │
                  └───────────┬────────────┘
                              │
             ┌────────────────┴────────────────┐
             ▼                                 ▼
   recallo.io (CNAME)                api.recallo.io (A Record)
  ┌──────────────────────┐          ┌───────────────────────────────────┐
  │   Vercel Edge        │          │   DigitalOcean Droplet (Ubuntu)   │
  │   (React Frontend)   │          │   IP: 104.248.xx.xx               │
  └──────────┬───────────┘          └─────────────────┬─────────────────┘
             │                                        │
             │ (HTTPS/WSS API Calls)                  │ (Port 80/443 TLS)
             └───────────────────────────────────────►│   Nginx Proxy
                                                      └───────┬─────────┘
                                                              │
                                            ┌─────────────────┴─────────┐
                                            ▼                           ▼
                                    api/v1/ws (WSS)             /api/* (HTTPS)
                                    ┌───────────────┐           ┌───────────────┐
                                    │  Go Backend   │◄─────────►│  Go Backend   │
                                    │ (Port 8080)   │           │ (Port 8080)   │
                                    └───────┬───────┘           └───────┬───────┘
                                            │                           │
                                            ▼                           ▼
                                      Local Redis               Neon Postgres
                                    (127.0.0.1:6379)         (External Cloud DB)
```

---

## 2. Step 1: DNS & Domain Routing

To connect the Vercel frontend (`recallo.io`) to the DigitalOcean backend (`api.recallo.io`), configure your DNS records at your domain registrar or DNS manager (e.g., Cloudflare, Namecheap):

1. **Frontend Routing:** Set a `CNAME` record for `@` (or `www`) pointing to `cname.vercel-dns.com` inside Vercel's domain configuration dashboard.
2. **Backend Routing:** Set an `A` record for `api` pointing to your DigitalOcean Droplet's public IPv4 address (e.g., `104.248.xx.xx`).

---

## 3. Step 2: Provisioning the DigitalOcean Droplet

Create a new Droplet in the DigitalOcean Console:

- **Image:** Ubuntu 24.04 LTS (x64)
- **Size:** Basic Shared CPU (1 CPU, 1 GB RAM, 25 GB SSD is sufficient for initial production; scale to 2 CPU / 4 GB RAM if concurrent WebRTC signalling loads increase).
- **Authentication:** SSH Keys (never use password authentication for production VPS).
- **Firewall:** Create a DigitalOcean Cloud Firewall allowing inbound TCP ports:
  - `22` (SSH) — Restricted to your IP address if possible.
  - `80` (HTTP) — Open to all.
  - `443` (HTTPS) — Open to all.

---

## 4. Step 3: Configuring CORS in the Go Backend

The Vercel client (`https://recallo.io`) makes cross-origin requests to the backend (`https://api.recallo.io`). If CORS is misconfigured, browsers will block HTTP requests and WebSocket connections.

Verify or add CORS middleware inside the Go router setup:

```go
func CORSMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        origin := r.Header.Get("Origin")
        if origin == "https://recallo.io" || origin == "https://www.recallo.io" {
            w.Header().Set("Access-Control-Allow-Origin", origin)
            w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS, PUT, DELETE")
            w.Header().Set("Access-Control-Allow-Headers", "Accept, Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization")
            w.Header().Set("Access-Control-Allow-Credentials", "true")
        }

        // Handle preflight OPTIONS request
        if r.Method == "OPTIONS" {
            w.WriteHeader(http.StatusNoContent)
            return
        }

        next.ServeHTTP(w, r)
    })
}
```

---

## 5. Step 4: Server Provisioning via SSH

SSH into the Droplet as `root`:

```bash
ssh root@104.248.xx.xx
```

Run the following initialization commands to set up the system dependencies:

```bash
# Update package definitions
apt-get update && apt-get upgrade -y

# Install Redis, Nginx, Certbot, and Go
apt-get install -y redis-server nginx certbot python3-certbot-nginx git build-essential

# Secure local Redis binding
# Ensure '/etc/redis/redis.conf' contains:
# bind 127.0.0.1 ::1
# requirepass <your-strong-redis-password>
systemctl enable redis-server
systemctl restart redis-server
```

Create a dedicated system user `deploy` to run the application binary without root privileges:

```bash
# Create deploy user
adduser --disabled-password --gecos "" deploy
usermod -aG sudo deploy

# Copy authorized SSH keys from root to deploy user
mkdir -p /home/deploy/.ssh
cp /root/.ssh/authorized_keys /home/deploy/.ssh/
chown -R deploy:deploy /home/deploy/.ssh
chmod 700 /home/deploy/.ssh
chmod 600 /home/deploy/.ssh/authorized_keys
```

---

## 6. Step 5: Managing the Go Process using PMGo

**PMGo** is a lightweight process manager for Go binaries. It runs them in the background, restarts them on crash, and redirects standard output and error streams to dedicated log files.

### Installing PMGo on the Droplet

Log in as the `deploy` user or install Go globally:

```bash
sudo wget https://go.dev/dl/go1.22.4.linux-amd64.tar.gz
sudo tar -C /usr/local -xzf go1.22.4.linux-amd64.tar.gz
export PATH=$PATH:/usr/local/go/bin
echo 'export PATH=$PATH:/usr/local/go/bin' >> /home/deploy/.bashrc
```

Install PMGo via Go's toolchain:

```bash
go install github.com/johntdyer/pmgo@latest
sudo cp /home/deploy/go/bin/pmgo /usr/local/bin/
```

### Initializing the Directory Structure and Environment Variables

Create the application directory:

```bash
sudo mkdir -p /opt/recallo/bin
sudo chown -R deploy:deploy /opt/recallo
```

Create a production environment configuration file `/opt/recallo/.env`:

```bash
cat << 'EOF' > /opt/recallo/.env
ENV=prod
DATABASE_URL=postgres://<neon_user>:<neon_password>@<neon_host>/recallo?sslmode=require
REDIS_URL=redis://:<your-strong-redis-password>@127.0.0.1:6379/0
HTTP_ADDRESS=127.0.0.1:8080
JWT_SECRET_KEY=<long-secure-random-jwt-key>
LIVEKIT_HOST=wss://<your-livekit-project>.livekit.cloud
LIVEKIT_API_KEY=<your-key>
LIVEKIT_API_SECRET=<your-secret>
LIVEKIT_WEBHOOK_SECRET=<your-webhook-secret>
DEEPGRAM_API_KEY=<your-deepgram-key>
SPACES_ENDPOINT=https://<region>.digitaloceanspaces.com
SPACES_BUCKET=<bucket-name>
SPACES_ACCESS_KEY=<spaces-key>
SPACES_SECRET_KEY=<spaces-secret>
OPENAI_API_KEY=<your-openai-key>
EOF
chmod 600 /opt/recallo/.env
chown deploy:deploy /opt/recallo/.env
```

### Starting the Process

Compile the production binary locally on your host or CI pipeline and transfer it to the server:

```bash
# On your local machine:
make build-prod
scp bin/recallo deploy@104.248.xx.xx:/opt/recallo/bin/recallo
```

On the Droplet, start the binary under PMGo:

```bash
# Run as 'deploy' user
cd /opt/recallo
pmgo start /opt/recallo/bin/recallo recallo-api
```

### PMGo Command Reference

- **List processes:** `pmgo list`
- **Stop process:** `pmgo stop recallo-api`
- **Restart process:** `pmgo restart recallo-api`
- **View logs:** `pmgo logs recallo-api`
- **Save current process list to auto-start:** `pmgo save`

---

## 7. Step 6: Nginx Reverse Proxy & SSL Setup

Configure Nginx to route traffic arriving on `api.recallo.io` to the local PMGo process running on port `8080`.

Create `/etc/nginx/sites-available/recallo`:

```nginx
server {
    listen 80;
    server_name api.recallo.io;
    return 301 https://$host$request_uri;
}

server {
    listen 443 ssl http2;
    server_name api.recallo.io;

    # Security headers
    add_header X-Frame-Options DENY;
    add_header X-Content-Type-Options nosniff;
    add_header Strict-Transport-Security "max-age=31536000" always;

    # Root API Requests
    location /api/ {
        proxy_pass         http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header   Host $host;
        proxy_set_header   X-Real-IP $remote_addr;
        proxy_set_header   X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_read_timeout 30s;
    }

    # WebSocket Protocol Upgrade Route
    location /api/v1/ws {
        proxy_pass         http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header   Upgrade $http_upgrade;
        proxy_set_header   Connection "upgrade";
        proxy_set_header   Host $host;
        proxy_read_timeout 3600s;
    }

    # LiveKit Webhook Endpoint
    location /webhooks/ {
        proxy_pass         http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header   Host $host;
        proxy_read_timeout 30s;
    }
}
```

Enable the configuration and reload Nginx:

```bash
sudo ln -sf /etc/nginx/sites-available/recallo /etc/nginx/sites-enabled/recallo
sudo rm -f /etc/nginx/sites-enabled/default
sudo nginx -t
sudo systemctl reload nginx
```

Obtain the SSL Certificate using Certbot:

```bash
sudo certbot --nginx -d api.recallo.io --non-interactive --agree-tos -m contact@recallo.io --redirect
```

---

## 8. Step 7: Local Deploy Script Automation

To deploy fresh updates without configuring complex CI pipelines, use this minimal deploy script from your development machine.

Save this script as `deploy.sh` on your local development machine:

```bash
#!/usr/bin/env bash
set -euo pipefail

# Configuration
SERVER_IP="104.248.xx.xx"
USER="deploy"
BINARY_DEST="/opt/recallo/bin/recallo"

echo "Building static binary..."
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -trimpath -o bin/recallo ./cmd/api

echo "Uploading binary to server..."
scp bin/recallo ${USER}@${SERVER_IP}:${BINARY_DEST}

echo "Restarting application process under PMGo..."
ssh ${USER}@${SERVER_IP} "pmgo restart recallo-api"

echo "Deployment complete."
```

---

## 9. Step 8: Automated CI/CD Pipeline with GitHub Actions

To replace manual local deploy scripts with a robust, automated production deployment pipeline, configure GitHub Actions to automatically test, compile, deploy via SCP, restart PMGo, and verify system health whenever changes are pushed to the `main` branch.

### 1. Required GitHub Repository Secrets

In your GitHub Repository, navigate to **Settings -> Secrets and variables -> Actions** and create the following repository secrets:

| Secret Name | Description & Example |
| :--- | :--- |
| `DROPLET_IP` | The public IPv4 address of your DigitalOcean Droplet (e.g., `104.248.xx.xx`). |
| `SSH_PRIVATE_KEY` | The private SSH key matching the public key authorized for the `deploy` user on the Droplet (`/home/deploy/.ssh/authorized_keys`). |
| `DOMAIN` | Your production backend API domain name without protocol (e.g., `api.recallo.io`). |

### 2. The Production CI/CD Workflow (`.github/workflows/deploy.yml`)

The repository includes the production workflow in `.github/workflows/deploy.yml`:

```yaml
name: Production CI/CD Deploy (PMGo)

on:
  push:
    branches: [main]
  workflow_dispatch:

jobs:
  test-and-deploy:
    runs-on: ubuntu-latest
    steps:
      - name: Checkout code
        uses: actions/checkout@v4

      - name: Setup Go environment
        uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true

      - name: Run unit & integration tests
        run: go test -v ./...

      - name: Build static production binary
        run: |
          CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
            go build -ldflags="-s -w" -trimpath -o bin/recallo ./cmd/api

      - name: Copy binary to DigitalOcean Droplet
        uses: appleboy/scp-action@v0.1.7
        with:
          host: ${{ secrets.DROPLET_IP }}
          username: deploy
          key: ${{ secrets.SSH_PRIVATE_KEY }}
          source: bin/recallo
          target: /opt/recallo/bin/
          strip_components: 1

      - name: Reload application process via PMGo
        uses: appleboy/ssh-action@v1.0.3
        with:
          host: ${{ secrets.DROPLET_IP }}
          username: deploy
          key: ${{ secrets.SSH_PRIVATE_KEY }}
          script: |
            export PATH=$PATH:/usr/local/bin:/usr/local/go/bin:$HOME/go/bin
            cd /opt/recallo
            chmod +x /opt/recallo/bin/recallo
            # Attempt to restart existing PMGo process; if not present, start a new instance
            pmgo restart recallo-api || pmgo start /opt/recallo/bin/recallo recallo-api
            pmgo save
            sleep 3
            # Verify process is running under PMGo
            pmgo list | grep -q "recallo-api" || (pmgo logs recallo-api && exit 1)

      - name: Post-deployment HTTP health check
        uses: appleboy/ssh-action@v1.0.3
        with:
          host: ${{ secrets.DROPLET_IP }}
          username: deploy
          key: ${{ secrets.SSH_PRIVATE_KEY }}
          script: |
            echo "Performing HTTP health check against https://${{ secrets.DOMAIN }}/api/v1/healthcheck..."
            curl -f --retry 5 --retry-delay 3 --retry-all-errors https://${{ secrets.DOMAIN }}/api/v1/healthcheck
The production workflow file is already configured at `.github/workflows/deploy.yml`. This section is the complete **activation and operational guide** — covering everything you need to do exactly once to make pushes to `main` automatically deploy to your Droplet without any manual steps.


### Mental Model: What Happens on Every `git push main`

```
[Your Local Machine]
      │
      │  git push origin main
      ▼
[GitHub Repository]
      │
      │  Triggers .github/workflows/deploy.yml
      ▼
[GitHub Actions Runner (ubuntu-latest)]
      │
      ├─ 1. Checkout source code
      ├─ 2. Set up Go toolchain (cached)
      ├─ 3. go test -v ./...            ◄ Fails here = deploy aborted, server untouched
      ├─ 4. go build → bin/recallo      ◄ Static binary, no dependencies
      ├─ 5. scp bin/recallo → Droplet   ◄ Copies binary to /opt/recallo/bin/
      ├─ 6. SSH: pmgo restart           ◄ Reloads process, zero-config
      └─ 7. SSH: curl healthcheck       ◄ Verifies app is actually alive
```


### Phase 1: One-Time SSH Key Setup for GitHub Actions

GitHub Actions needs a private SSH key to log into your Droplet as the `deploy` user. You generate one dedicated key pair for this purpose only.

#### Step 1.1 — Generate a Dedicated CI Key on Your Droplet (as root)

SSH into your Droplet as `root` and run:

```bash
# Generate a dedicated ed25519 key for GitHub Actions
ssh-keygen -t ed25519 -C "github-actions-recallo" -f /root/.ssh/recallo_ci_key -N ""

# Authorize it for the deploy user (write directly, no copy-paste corruption)
mkdir -p /home/deploy/.ssh
cat /root/.ssh/recallo_ci_key.pub >> /home/deploy/.ssh/authorized_keys

# Lock down permissions (SSH is strict — wrong perms = silent rejection)
chown -R deploy:deploy /home/deploy/.ssh
chmod 700 /home/deploy/.ssh
chmod 600 /home/deploy/.ssh/authorized_keys

# Fedora/RHEL: restore SELinux context
restorecon -R /home/deploy/.ssh 2>/dev/null || true
```

#### Step 1.2 — Verify the Key Works from the Droplet Itself

```bash
# Test SSH login as deploy using the new key (local loopback test)
ssh -o StrictHostKeyChecking=no -i /root/.ssh/recallo_ci_key deploy@127.0.0.1 "echo SUCCESS"
```

Expected output: `SUCCESS`. If you see `Permission denied`, check `/var/log/secure` (Fedora) or run `sshd -T | grep pubkey` to verify `PubkeyAuthentication yes` is set.

#### Step 1.3 — Print the Private Key to Copy into GitHub

```bash
cat /root/.ssh/recallo_ci_key
```

Copy the **entire output** including `-----BEGIN OPENSSH PRIVATE KEY-----` and `-----END OPENSSH PRIVATE KEY-----`.

---

### Phase 2: Configure GitHub Repository Secrets

These three secrets are the only configuration GitHub Actions needs. You set them once and never touch them again unless the Droplet IP or domain changes.

Navigate to your GitHub repo → **Settings** → **Secrets and variables** → **Actions** → **New repository secret**.

| Secret Name | What to Put In | Where to Get It |
| :--- | :--- | :--- |
| `DROPLET_IP` | Your Droplet's public IPv4 (e.g. `143.110.177.227`) | DigitalOcean Console → Droplets |
| `SSH_PRIVATE_KEY` | The full private key content printed in Step 1.3 | Copied from `cat /root/.ssh/recallo_ci_key` |
| `DOMAIN` | Your backend domain without protocol (e.g. `api.recallo.io`) | Your DNS configuration |

> **Important:** `SSH_PRIVATE_KEY` must be pasted exactly as printed — no extra spaces, no missing lines. The leading dashes and trailing newline must be preserved.

---

### Phase 3: First Deployment — Getting Your Project Live

This is the sequence you follow once, the first time you deploy a fresh Droplet. After this, every `git push main` does all of this automatically.

#### Step 3.1 — Ensure the App Directory Exists on the Droplet

```bash
# On the Droplet as root
mkdir -p /opt/recallo/bin
chown -R deploy:deploy /opt/recallo
```

#### Step 3.2 — Upload Your Environment File

This file is **never committed to Git** (it contains secrets). Copy it from your local machine to the Droplet manually once:

```bash
# From your local machine
scp /path/to/your/.env root@143.110.177.227:/opt/recallo/.env

# On the Droplet: lock down ownership
chown deploy:deploy /opt/recallo/.env
chmod 600 /opt/recallo/.env
```

The `.env` file must contain all the variables listed in `env.example` at the root of the repository.

#### Step 3.3 — Trigger the First Automated Deploy

Push any commit to `main` to kick off the pipeline:

```bash
git add .
git commit -m "chore: trigger first production deployment"
git push origin main
```

Go to your GitHub repository → **Actions** tab to watch the pipeline run in real-time. Every step's logs are visible.

#### Step 3.4 — Start the PMGo Process the First Time (if pipeline hasn't done it)

The deploy workflow handles this, but if you need to start manually on the Droplet for the very first time:

```bash
# As the deploy user on the Droplet
export PATH=$PATH:/usr/local/bin:/usr/local/go/bin:$HOME/go/bin
cd /opt/recallo
pmgo start /opt/recallo/bin/recallo recallo-api
pmgo save   # Persist the process list across reboots
```

---

### Phase 4: Day-to-Day Developer Workflow

After the one-time setup above, your full development and deployment loop is:

```bash
# 1. Write code locally
# 2. Run locally to verify
make dev

# 3. Commit and push — deployment is fully automated
git add .
git commit -m "feat: your feature description"
git push origin main
# → GitHub Actions runs tests, builds, deploys, and health-checks automatically
```

**You never manually SSH to deploy again.** The only reasons to SSH into the Droplet directly are:
- Checking live logs: `pmgo logs recallo-api`
- Updating the `.env` file when secrets rotate
- Investigating system-level issues (disk, memory, Redis)

---

### Phase 5: Monitoring & Verification Commands

After each deployment, the GitHub Actions health check confirms the API is live. For manual inspection:

```bash
# Check PMGo process status
pmgo list

# Stream live application logs
pmgo logs recallo-api

# Manual HTTP health check
curl -sf https://api.recallo.io/api/v1/healthcheck

# Check Nginx status
sudo systemctl status nginx

# Check Redis
redis-cli -a <your-redis-password> ping
```

---

### Troubleshooting Decision Tree

```
Deployment failed in GitHub Actions?
│
├─ Failed at "Run unit & integration tests"
│     → Fix the failing test. The server was NOT touched.
│
├─ Failed at "Copy binary to Droplet"
│     → SSH key rejected. Verify SSH_PRIVATE_KEY secret is correct.
│        On Droplet: cat /home/deploy/.ssh/authorized_keys
│        Compare with: cat /root/.ssh/recallo_ci_key.pub
│
├─ Failed at "Reload application process via PMGo"
│     → PMGo path issue or binary permission issue.
│        On Droplet: which pmgo, ls -la /opt/recallo/bin/recallo
│        Fix: chmod +x /opt/recallo/bin/recallo
│
└─ Failed at "Post-deployment health check"
      → App crashed on startup. Check logs:
         pmgo logs recallo-api
         Common cause: missing or wrong value in /opt/recallo/.env
```

---

## 10. Step 9: Droplet Extensions & Scalability Boundaries

When scaling becomes necessary, transition your architecture using these logical steps:

### When to scale vertically (Upgrading the Droplet)

- **Symptom:** CPU usage is consistently above 70% during peak calling hours, or system memory exhaustion causes Go's OOM killer to terminate the process.
- **Action:** Scale the Droplet size in the DigitalOcean panel (requires a 2-minute restart). A 2 CPU, 4 GB RAM Droplet can handle thousands of concurrent WebSocket connections because Go runtime handles connection multiplexing efficiently.

### When to scale horizontally (Adding a Load Balancer)

If you require zero-downtime rolling deploys or need to scale beyond a single physical machine:

1. **Provision a second Droplet** running the exact same PMGo setup.
2. **Move Redis to a managed cluster or single dedicated node** so that both instances share the same job queue, PubSub channels, and typing indicators.
3. **Provision a DigitalOcean Load Balancer** in front of your Droplets.
4. **Point the `A` record of `api.recallo.io`** to the Load Balancer IP rather than the individual Droplet IPs. Nginx on the load balancer handles TLS termination and forwards traffic to the backend nodes.
