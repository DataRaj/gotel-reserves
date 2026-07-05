# SSH Public Key Authentication Debugging: Fedora + SELinux

This guide documents the exact diagnostic and fix process for resolving
`Permission denied (publickey,gssapi-keyex,gssapi-with-mic)` errors on a
Fedora DigitalOcean Droplet with SELinux enforcing mode, specifically for
the `deploy` user used by the GitHub Actions CI/CD pipeline.

---

## Root Causes Checklist (Fedora / RHEL-specific)

Unlike Ubuntu/Debian, Fedora introduces three additional failure modes beyond
simple permission issues:

| # | Root Cause | Ubuntu? | Fedora? |
|---|---|---|---|
| 1 | Wrong file permissions on `.ssh/` or `authorized_keys` | ✅ | ✅ |
| 2 | Wrong file owner on `.ssh/` or `authorized_keys` | ✅ | ✅ |
| 3 | SELinux security context incorrect on `.ssh/` | ❌ | ✅ |
| 4 | `sshd_config` missing `PubkeyAuthentication yes` | ✅ | ✅ |
| 5 | `AuthorizedKeysFile` points to a non-existent path | ✅ | ✅ |
| 6 | `AuthorizedKeysCommand` override (SSSD/LDAP integration) | ❌ | ✅ |
| 7 | Key algorithm not permitted (`ed25519` blocked by FIPS policy) | ❌ | ✅ |
| 8 | Firewall / SELinux blocking sshd port binding | ❌ | ✅ |

---

## Step 1: Read the Real Rejection Reason from sshd Logs

On Fedora, SSH auth logs are **NOT** in `/var/log/auth.log` (that is Debian/Ubuntu).

### Option A — journalctl (preferred on Fedora with systemd-journald)

```bash
# As root on the droplet, watch live SSH auth events:
journalctl -u sshd -f

# Then, from your local machine, attempt the failing login:
ssh -i /root/.ssh/recallo_ci_key deploy@143.110.177.227

# You will see the exact rejection reason in journalctl output.
```

### Option B — /var/log/secure (older Fedora / rsyslog still present)

```bash
# Check if rsyslog is writing to /var/log/secure:
ls -la /var/log/secure

# If the file exists, tail it during a login attempt:
tail -f /var/log/secure
```

### Option C — Enable verbose sshd debug logging temporarily

> **Warning:** Only do this temporarily. Revert `LogLevel` to `INFO` after debugging.

```bash
# Edit sshd_config:
sudo nano /etc/ssh/sshd_config

# Change or add:
LogLevel DEBUG3

# Restart sshd (non-interrupting reload for Fedora):
sudo systemctl reload sshd

# Now watch the journal:
journalctl -u sshd -f
```

The `DEBUG3` output will show lines like:

```
Authentication refused: bad ownership or modes for directory /home/deploy/.ssh
```
or
```
Could not open authorized keys '/home/deploy/.ssh/authorized_keys': Permission denied
```
These lines pinpoint the exact failing check.

---

## Step 2: Verify sshd_config Required Settings

```bash
sudo grep -E \
  'PubkeyAuthentication|AuthorizedKeysFile|PasswordAuthentication|PermitRootLogin|UsePAM|AuthorizedKeysCommand' \
  /etc/ssh/sshd_config
```

**Required values:**

```ini
PubkeyAuthentication yes
AuthorizedKeysFile  .ssh/authorized_keys
PasswordAuthentication no
UsePAM yes
```

> **Important:** If `AuthorizedKeysCommand` is set (e.g., to `/usr/bin/sss_ssh_authorizedkeys`),
> sshd is querying SSSD/LDAP for keys instead of reading `authorized_keys`
> from disk. This overrides file-based auth entirely.
> Disable it if not using centralized identity management:
> ```ini
> AuthorizedKeysCommand none
> AuthorizedKeysCommandUser nobody
> ```

If you make any changes to `sshd_config`:

```bash
sudo sshd -t        # Test config syntax first
sudo systemctl reload sshd
```

---

## Step 3: Fix SELinux Security Context (Most Common Fedora Cause)

Even with correct Unix permissions (700/600), SELinux can block `sshd` from
reading `authorized_keys` if the file has the wrong security label.

### 3a. Check Current SELinux Context

```bash
ls -laZ /home/deploy/.ssh/
```

**Expected output** (labels must include `ssh_home_t`):

```
drwx------. deploy deploy unconfined_u:object_r:ssh_home_t:s0 .
drwx------. deploy deploy unconfined_u:object_r:user_home_dir_t:s0 ..
-rw-------. deploy deploy unconfined_u:object_r:ssh_home_t:s0 authorized_keys
```

The critical label is `ssh_home_t`. If you see `default_t`, `unlabeled_t`,
`user_home_t`, or anything other than `ssh_home_t` on `authorized_keys`,
SELinux will silently deny `sshd` access regardless of Unix permissions.

### 3b. Restore the Correct SELinux Context

```bash
# Restore default SELinux labels recursively:
sudo restorecon -R -v /home/deploy /home/deploy/.ssh

# Verify the label is now ssh_home_t:
ls -laZ /home/deploy/.ssh/authorized_keys
```

### 3c. If restorecon Doesn't Fix It — Manually Set the Label

If `restorecon` doesn't apply `ssh_home_t` (e.g., because the file was
created by root and the policy DB is confused), force the correct label:

```bash
sudo chcon -R -t ssh_home_t /home/deploy/.ssh
sudo chcon -t user_home_dir_t /home/deploy

# Verify:
ls -laZ /home/deploy/.ssh/
```

### 3d. Check SELinux AVC Denials (Definitive Diagnosis)

```bash
# Search audit log for SSH-related AVC denials:
sudo ausearch -c 'sshd' -m avc -ts recent

# Also check the general audit log:
sudo tail -50 /var/log/audit/audit.log | grep -E 'sshd|ssh_home'
```

An AVC denial looks like:

```
type=AVC msg=audit(1234567890.123:456): avc:  denied  { open } for  pid=12345
comm="sshd" name="authorized_keys" dev="vda1" ino=12345
scontext=system_u:system_r:sshd_t:s0-s0:c0.c1023
tcontext=unconfined_u:object_r:default_t:s0
tclass=file permissive=0
```

The `tcontext` field shows the current (wrong) label. `permissive=0` confirms
SELinux actively blocked access (not just logged it).

---

## Step 4: Verify the Key Was Added Correctly

```bash
# On the droplet as root or deploy:
cat /home/deploy/.ssh/authorized_keys

# The entry must be on a SINGLE LINE:
# ssh-ed25519 AAAA...base64... optional-comment
# ssh-rsa AAAA...base64... optional-comment
```

> **Caution:** A common mistake when copying keys from `/root/.ssh/authorized_keys`
> is accidentally introducing line breaks. Check with:
> ```bash
> wc -l /home/deploy/.ssh/authorized_keys   # Should equal number of keys
> cat -A /home/deploy/.ssh/authorized_keys  # ^M means Windows CRLF line endings
> ```
> Fix Windows CRLF if present:
> ```bash
> sudo sed -i 's/\r//' /home/deploy/.ssh/authorized_keys
> ```

---

## Step 5: Check Fedora's Crypto Policy (FIPS Mode)

Fedora enforces system-wide cryptographic policies. If the Droplet was
provisioned with FIPS mode enabled, some key algorithms are rejected.

```bash
# Check current crypto policy:
sudo update-crypto-policies --show

# Common values:
# DEFAULT   → accepts RSA-2048+, ECDSA, Ed25519 (fine for CI)
# FIPS      → rejects Ed25519 (not FIPS-certified), requires RSA-3072+
# LEGACY    → broader compatibility
```

**If output is `FIPS`:**

Ed25519 keys are rejected. Use RSA 4096 instead:

```bash
# Generate a FIPS-compatible RSA key:
ssh-keygen -t rsa -b 4096 -f /root/.ssh/recallo_ci_key_rsa -C "github-actions-deploy"
```

Or relax the policy to `DEFAULT` (only if FIPS compliance is not required):

```bash
sudo update-crypto-policies --set DEFAULT
sudo systemctl restart sshd
```

---

## Step 6: Verify SSH Daemon is Listening and Accessible

```bash
# Confirm sshd is active:
sudo systemctl status sshd

# Confirm it is listening on port 22:
sudo ss -tlnp | grep ':22'

# Fedora uses firewalld — NOT ufw. Verify SSH is allowed:
sudo firewall-cmd --list-services
# 'ssh' must appear in output

# If ssh is not listed, add it permanently:
sudo firewall-cmd --permanent --add-service=ssh
sudo firewall-cmd --reload
```

> **Note:** DigitalOcean Droplets have a Cloud Firewall separate from
> `firewalld`. Ensure port 22 is open in both the DigitalOcean Cloud Firewall
> AND in `firewalld` on the OS.

---

## Step 7: Test with Maximum SSH Client Verbosity

From your local machine (or wherever the CI key lives):

```bash
ssh -vvv -i /root/.ssh/recallo_ci_key deploy@143.110.177.227
```

Watch for these output patterns:

```
# GOOD: key offered and accepted
debug1: Offering public key: /root/.ssh/recallo_ci_key ED25519 SHA256:...
debug1: Server accepts key: ...
debug1: Authentication succeeded (publickey).

# BAD: key offered but silently rejected (server-side issue)
debug1: Offering public key: /root/.ssh/recallo_ci_key ED25519 SHA256:...
debug1: Authentications that can continue: publickey,gssapi-keyex,gssapi-with-mic
# (No "Server accepts key" line = server rejected key silently)
```

If the key is offered but not accepted → issue is server-side (SELinux,
permissions, sshd_config, or key mismatch).

If the key is not even offered → issue is client-side (wrong path or key format).

---

## Step 8: Complete Fix Sequence (Run as root on Droplet)

Run these commands in order after identifying the root cause:

```bash
# 1. Ensure deploy user and .ssh directory exist:
id deploy || useradd -m -s /bin/bash deploy
mkdir -p /home/deploy/.ssh

# 2. Add the CI public key to authorized_keys (single line):
#    Replace the placeholder with your actual public key
echo "ssh-ed25519 AAAA...your-ci-public-key... github-actions-deploy" \
  >> /home/deploy/.ssh/authorized_keys

# 3. Correct Unix ownership and permissions:
chown -R deploy:deploy /home/deploy/.ssh
chmod 700 /home/deploy/.ssh
chmod 600 /home/deploy/.ssh/authorized_keys

# 4. Fix SELinux labels — THE CRITICAL FEDORA STEP:
restorecon -R -v /home/deploy

# 5. Verify label is now ssh_home_t:
ls -laZ /home/deploy/.ssh/authorized_keys
# Must show: unconfined_u:object_r:ssh_home_t:s0

# 6. If still wrong label, force it:
chcon -R -t ssh_home_t /home/deploy/.ssh
chcon -t user_home_dir_t /home/deploy

# 7. Confirm PubkeyAuthentication is enabled in sshd_config:
grep 'PubkeyAuthentication' /etc/ssh/sshd_config
# Expected: PubkeyAuthentication yes

# 8. Test config and reload sshd:
sshd -t && systemctl reload sshd

# 9. Watch live logs during test login:
journalctl -u sshd -f &
# Then from your local machine:
# ssh -vvv -i /path/to/recallo_ci_key deploy@143.110.177.227
```

---

## Step 9: Authorizing the Key for GitHub Actions

Once the key authenticates successfully, add it to GitHub Repository Secrets:

1. **Copy the private key content:**
   ```bash
   cat /root/.ssh/recallo_ci_key
   ```
2. In GitHub: **Settings → Secrets and variables → Actions → New repository secret**
3. **Name:** `SSH_PRIVATE_KEY`
4. **Value:** Paste the entire private key including the `-----BEGIN...-----`
   and `-----END...-----` header/footer lines.

> **Important:** The `deploy.yml` workflow connects as `username: deploy`.
> The matching public key (`recallo_ci_key.pub`) must be in
> `/home/deploy/.ssh/authorized_keys`, **not** in `/root/.ssh/authorized_keys`.

---

## Quick Reference: Fedora vs Ubuntu SSH Differences

| Aspect | Ubuntu / Debian | Fedora / RHEL |
|---|---|---|
| SSH auth log | `/var/log/auth.log` | `journalctl -u sshd` or `/var/log/secure` |
| SELinux | Not enforced by default | Enforcing by default |
| Firewall tool | `ufw` | `firewalld` (`firewall-cmd`) |
| Package manager | `apt` | `dnf` |
| sshd service name | `ssh` | `sshd` |
| Crypto policy | N/A | `update-crypto-policies` |
| `authorized_keys` SELinux label | N/A (no enforcement) | Must be `ssh_home_t` |
| SSSD override risk | Low | Common in cloud images |
| Reload command | `systemctl reload ssh` | `systemctl reload sshd` |
