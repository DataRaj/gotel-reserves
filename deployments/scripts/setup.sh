#!/usr/bin/env bash
set -euo pipefail

# Run as root on a fresh DigitalOcean Ubuntu droplet.
# Usage: bash setup.sh <your-domain>
DOMAIN="${1:?Usage: setup.sh <domain>}"

# ── 1. Deploy user ─────────────────────────────────────────────────────────────
id deploy &>/dev/null || adduser --disabled-password --gecos "" deploy
usermod -aG sudo deploy
[ -d /home/deploy/.ssh ] || rsync --archive --chown=deploy:deploy ~/.ssh /home/deploy

# Allow deploy user to restart the service without a password for CI/CD
echo "deploy ALL=(ALL) NOPASSWD: /bin/systemctl restart recallo-api, /bin/systemctl is-active recallo-api" > /etc/sudoers.d/deploy-recallo

# ── 2. Firewall ────────────────────────────────────────────────────────────────
ufw allow 22/tcp
ufw allow 80/tcp
ufw allow 443/tcp
ufw --force enable

# ── 3. Packages ────────────────────────────────────────────────────────────────
apt-get update -q
apt-get install -y -q redis-server nginx certbot python3-certbot-nginx

# ── 4. Secure Redis ────────────────────────────────────────────────────────────
REDIS_CONF=/etc/redis/redis.conf
sed -i 's/^bind .*/bind 127.0.0.1/' "$REDIS_CONF"
if ! grep -q "^requirepass " "$REDIS_CONF"; then
    REDIS_PASS=$(openssl rand -hex 32)
    echo "requirepass $REDIS_PASS" >> "$REDIS_CONF"
    echo "[setup] Redis password: $REDIS_PASS  — add to /opt/recallo/config/dev.env as REDIS_URL"
fi
systemctl enable redis-server
systemctl restart redis-server

# ── 5. App directories ────────────────────────────────────────────────────────
mkdir -p /opt/recallo/bin /opt/recallo/config
chown -R deploy:deploy /opt/recallo

# ── 6. Nginx ──────────────────────────────────────────────────────────────────
NGINX_CONF=/etc/nginx/sites-available/recallo
cp "$(dirname "$0")/../nginx/recallo" "$NGINX_CONF"
sed -i "s/api.yourdomain.com/$DOMAIN/g" "$NGINX_CONF"
ln -sf "$NGINX_CONF" /etc/nginx/sites-enabled/recallo
rm -f /etc/nginx/sites-enabled/default
nginx -t
systemctl reload nginx

# ── 7. TLS ────────────────────────────────────────────────────────────────────
certbot --nginx -d "$DOMAIN" --non-interactive --agree-tos -m "ops@$DOMAIN" --redirect

# ── 8. Systemd unit ───────────────────────────────────────────────────────────
cp "$(dirname "$0")/../systemd/recallo-api.service" /etc/systemd/system/
systemctl daemon-reload
systemctl enable recallo-api

echo ""
echo "[setup] Done. Next:"
echo "  1. SCP your environment file to /opt/recallo/config/dev.env"
echo "  2. GitHub Actions will handle SCPing the binary and restarting the service automatically."
echo "  3. Check logs: journalctl -u recallo-api -f"
