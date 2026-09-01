[中文](DEPLOY.md)

# MITMRouter Deployment Guide

> Applies to: the `main` branch. The database uses idempotent table creation and has no migration scripts; upgrades only require replacing the binary and restarting it.
> This document records a complete production deployment procedure (reference environment: an Ubuntu 24.04 VPS),
> and can be used as a runbook for deploying to a new machine or upgrading an existing installation.
> The placeholders `<SERVER_IP>` and `<DOMAIN>` are used throughout; replace them with actual values when deploying.

---

## 1. Architecture and Port Conventions

| Component | Description |
|---|---|
| Ingress listener | `0.0.0.0:55666`, TLS-only (once a certificate is configured, plaintext connections are rejected during the handshake), with inbound Basic authentication |
| Admin console listener | `0.0.0.0:55667`, TLS-only, with session Cookie authentication |
| Data directory | `/opt/mitmrouter/data` (SQLite, containing the CA, password, settings, and audit data). At startup, the program automatically tightens permissions: directory `0700`, database and WAL/SHM files `0600` |
| Certificates | Let's Encrypt (`certbot`); copy the certificates to `/opt/mitmrouter/certs/` |

Both listener addresses are **startup parameters** and are not stored in the database; all other runtime configuration is stored in SQLite and supports hot reloads.

## 2. Local Build

```bash
# Default output: bin/mitmrouter-linux-amd64, a static, stripped binary with no CGO dependency
./scripts/build.sh
```

The script always runs in this order: install frontend dependencies from the lockfile → build the admin console into `internal/webui/dist` → compile the Go binary using
`go:embed`. For cross-compilation, specify the target with arguments, for example:

```bash
./scripts/build.sh --os linux --arch arm64
```

## 3. Server Initialization

```bash
ssh root@<SERVER_IP>
mkdir -p /opt/mitmrouter/{bin,data,certs}
```

### 3.1 Certificates

Prerequisite: certbot has issued a certificate for the server's domain. Confirm which domain resolves to this machine:

```bash
getent hosts <DOMAIN>                    # Should return this machine's public IP
openssl x509 -in /etc/letsencrypt/live/<DOMAIN>/cert.pem -noout -enddate -ext subjectAltName
```

Copy **dereferenced copies** into the deployment directory (`live/` contains symlinks to `archive/`):

```bash
cp -L /etc/letsencrypt/live/<DOMAIN>/fullchain.pem /opt/mitmrouter/certs/fullchain.pem
cp -L /etc/letsencrypt/live/<DOMAIN>/privkey.pem   /opt/mitmrouter/certs/privkey.pem
chmod 600 /opt/mitmrouter/certs/privkey.pem
```

Install a renewal hook to synchronize the copies automatically after certbot renews them; the application hot-reloads when it detects an mtime change, so no restart is needed:

```bash
cat > /etc/letsencrypt/renewal-hooks/deploy/mitmrouter-certs.sh <<'EOF'
#!/bin/sh
cp -L /etc/letsencrypt/live/<DOMAIN>/fullchain.pem /opt/mitmrouter/certs/fullchain.pem
cp -L /etc/letsencrypt/live/<DOMAIN>/privkey.pem   /opt/mitmrouter/certs/privkey.pem
chmod 600 /opt/mitmrouter/certs/privkey.pem
EOF
chmod +x /etc/letsencrypt/renewal-hooks/deploy/mitmrouter-certs.sh
```

### 3.2 systemd Unit

```ini
# /etc/systemd/system/mitmrouter.service
[Unit]
Description=MITMRouter ingress + admin console
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/opt/mitmrouter/bin/mitmrouter -data /opt/mitmrouter/data -addr 0.0.0.0:55666 -admin-addr 0.0.0.0:55667
Restart=on-failure
RestartSec=2

[Install]
WantedBy=multi-user.target
```

### 3.3 Firewall: Allow Only Same-Host Docker Containers to Access the Ingress

If the ingress is used only by a Docker service on the **same server** (for example, Sub2API), do not expose ingress port `55666/tcp` to the public internet. When a container accesses `<DOMAIN>:55666`, traffic enters the host through the Docker bridge; therefore, even if the domain resolves to the host's public IP, UFW still sees it as inbound traffic from the Docker subnet.

When using UFW, allow only the Docker private network range to reach the ingress port:

```bash
# First confirm the actual Docker subnet; the example container address is 172.18.x.x
# Example output: 172.18.0.0/16
docker network inspect <DOCKER_NETWORK> \
  --format '{{range .IPAM.Config}}{{.Subnet}}{{end}}'

# Docker's default address pool is usually within 172.16.0.0/12;
# you can also narrow this to the actual subnet found above.
ufw allow from 172.16.0.0/12 to any port 55666 proto tcp

# Do not run `ufw allow 55666/tcp`: it would expose the ingress to the public internet.
ufw status numbered
```

> If Docker uses a custom address pool, replace `172.16.0.0/12` with the actual container subnet. This rule is only for inbound ingress port `55666/tcp`; configure admin port `55667/tcp` according to a separate operations access policy.

Verify that same-host containers can connect while the public internet still cannot:

```bash
# This should succeed inside a same-host container (whether -k is needed depends on its trusted root certificates)
docker exec <SUB2API_CONTAINER> curl -k --connect-timeout 5 --max-time 15 \
  -o /dev/null -s -w '%{http_code}\n' https://<DOMAIN>:55666/

# Testing from another machine should time out or be refused
curl -k --connect-timeout 5 --max-time 15 https://<DOMAIN>:55666/
```

## 4. Deployment Procedure (Fresh Installation)

1. **Upload the binary**: `scp bin/mitmrouter-linux-amd64 root@<SERVER_IP>:/opt/mitmrouter/bin/mitmrouter && chmod +x …`
2. **First-start bootstrap** (there is no TLS configuration yet, so start once in plaintext to create the database):
   ```bash
   systemctl daemon-reload && systemctl enable --now mitmrouter
   # Capture the one-time administrator password (it appears only in the logs)
   journalctl -u mitmrouter | grep "Admin password:"
   ```
3. **Complete all configuration in the web interface** (admin console: `https://<DOMAIN>:55667/ui/`; log in with the one-time password from the previous step).
   Save all runtime configuration through the admin console; **do not edit the database manually**:

   - **TLS certificate paths**: In the Basic Settings page → the four Ingress/Admin TLS certificate fields, enter `/opt/mitmrouter/certs/fullchain.pem` and `/opt/mitmrouter/certs/privkey.pem`, respectively (both must be provided to enable TLS; after saving, the page will indicate that a restart is required):
     ```bash
     systemctl restart mitmrouter
     ```
   - **Inbound authentication**: In the Basic Settings page → Inbound Authentication, enter `<USERNAME>:<PASSWORD>` and save; it takes effect immediately (the saved username and password are shown directly in the logged-in admin console, and the ingress address also includes the real authentication information; do not disclose them);
   - All other settings (default upstream, no-Marker policy, allow/block lists, log retention, and so on) take effect as soon as they are saved on their respective pages.

   > The only change that requires a restart is a TLS path change (the listeners bind their certificates at startup); updates to certificate contents at the same paths (such as certbot renewals) are handled by hot reload and require no restart.

## 5. Verification Checklist

```bash
systemctl is-active mitmrouter                       # active
ss -tlnp | grep -E ":5566[67]"                        # both ports listening
journalctl -u mitmrouter -n 1 | grep started          # ingress_tls/admin_tls should be true
                                                       # inbound_auth reflects the authentication setting
# Verify the external certificate (omitting -k is the real verification)
curl -s -o /dev/null -w "%{http_code}\n" https://<DOMAIN>:55667/ui/     # 200

# Ingress connectivity: no credentials should yield 407; credentials should return the egress IP
curl -x https://<DOMAIN>:55666 https://api.ipify.org          # Expected to fail (407)
curl -x "https://<USERNAME>:<PASSWORD>@<DOMAIN>:55666" https://api.ipify.org
```

## 6. Trusting the MITM Root CA on Clients

By default, an empty ACL allowlist means **all HTTPS targets are intercepted and parsed**. Clients must trust the deployment-generated root CA or they will report a self-signed certificate error. Download the CA from the admin console: `GET /api/ca.pem` (PEM) / `/api/ca.crt` (DER).

### Linux Host (Non-Container)

```bash
cp ca.pem /usr/local/share/ca-certificates/mitm-router-root-ca.crt
update-ca-certificates
```

### Containers: Merge on the Host + Mount Read-Only (Standard Practice)

For every container that needs outbound access through the ingress, **build its own bundle** (using the system root certificates from its own image as the base), generate it on the host, and map it back into that container with a read-only bind mount (overriding the container's `/etc/ssl/certs/ca-certificates.crt`). Do not append anything inside the container—changes made with `docker exec` are lost when the container is recreated.

```bash
# ① Prepare the MITM root certificate: download ca.pem from the admin console and save it in a fixed location (every container uses the same root)
mkdir -p /opt/mitmrouter/client-ca
#    Upload ca.pem as /opt/mitmrouter/client-ca/mitm-root-ca.pem

# ② Run one round separately for each container to produce its own bundle file
docker exec <CONTAINER> cat /etc/ssl/certs/ca-certificates.crt \
  > /opt/mitmrouter/client-ca/<CONTAINER_NAME>.crt    # Base this on the system root certificates of that container image
cat /opt/mitmrouter/client-ca/mitm-root-ca.pem \
  >> /opt/mitmrouter/client-ca/<CONTAINER_NAME>.crt   # Append the MITM root certificate (PEM)
```

In each container's service definition, map **its own** file:

```yaml
    volumes:
      - /opt/mitmrouter/client-ca/<CONTAINER_NAME>.crt:/etc/ssl/certs/ca-certificates.crt:ro
```

Key points:

- **Do not reuse a bundle across containers**: different base images have different sets of system root certificates; each container's own set is authoritative;
- **When a container's bundle must be regenerated**: after its base image is upgraded and its system root certificates change; or after the instance is reset or the root CA is replaced (§8, in which case all containers must be rebuilt). certbot renews the listener TLS certificates (§3.1), which are unrelated to this file;
- Restart the container after the mount is applied so it rereads the file; running processes do not hot-load it.

> Principle: under Alpine/Debian, Go/OpenSSL's default verification path is this single file; merging the MITM root certificate (in PEM format) into it completes the trust chain.

### Verify Ingress Availability Inside a Container (Only Without `-k` Does This Verify the Trust Chain)

```bash
docker exec <CONTAINER> curl --max-time 30 \
  -x "https://<USERNAME>:<PASSWORD>@<DOMAIN>:55666" https://api.ipify.org
```

## 7. Upstream Configuration

Admin console “Upstream Egress List” → Add. Field notes:

| Platform | base_url username syntax | Description |
|---|---|---|
| dataimpulse | `<login>__cr.us` | Automatically appends `;sessid.<IDENTITY>` as the session parameter |
| decodo | Must start with `user-` | Injects `-session-` / `-sessionduration-` |
| 1024proxy | `<apikey>-region-US-sid-x-t-5` | The application rewrites sid/t; t is controlled by session_ttl_min |
| resin | `Platform[.Account]`, with the password as the token | Rewrites the username to `Platform.<IDENTITY>` |
| generic | Any + inject template | `{user}/{sid}/{ttl_min}/{country}` placeholders |

> **The platform must match the provider**: it determines the username-rewriting syntax. If it is wrong, authentication will usually still succeed (the gateway only recognizes the API key + password), making it appear to “work,” but session-parameter injection will be ineffective and sticky routing will actually fail.
> Change the platform by editing and saving it in the admin console; the change takes effect hot without a restart.

### 7.1 Account Mapping and Sync Sources (Optional)

When credentials are API keys (`sk-…`, etc.), this page is unnecessary—the service uses the key hash directly for sticky routing. If the upstream account uses Bearer AT/RT (subscription accounts), it is recommended to create a mapping on the “Account Mapping” page and attach the sticky identity to the **account**; rotating refreshed tokens then leaves the egress IP unchanged.

- **Pull from a sync source**: Add a source instance (choose CLIProxyAPI or Sub2API), enter the instance address and credentials, and the built-in synchronizer will periodically pull that service's account AT/RT; multiple instances of the same source type are supported.
- **Manual registration**: Enter the account identifier (email/uuid) and its AT/RT directly.
- Disable the feature globally with `acctmap_enabled=false`; all traffic then falls back to pure Marker-hash routing.

You can skip this section when subscription accounts are not used.

## 8. Upgrade / Rollback

**Upgrade** (the schema uses idempotent table creation, so replace the binary directly across versions):

```bash
scp bin/mitmrouter-linux-amd64 root@<SERVER_IP>:/opt/mitmrouter/bin/mitmrouter.new
ssh root@<SERVER_IP> 'cd /opt/mitmrouter/bin && mv mitmrouter mitmrouter.bak && mv mitmrouter.new mitmrouter && systemctl restart mitmrouter'
```

**Rollback**: Repeat the steps above in reverse; the data directory is unaffected.

**Reset the entire instance** (permitted before going live): `systemctl stop mitmrouter && rm -rf /opt/mitmrouter/data/*`
Starting it again launches a fresh bootstrap (new CA and new administrator password; trust relationships with the old CA become invalid).

## 9. Troubleshooting Quick Reference

| Symptom | Diagnosis |
|---|---|
| Plaintext connection refused | This listener has TLS-only enabled; this is expected |
| curl shows 000 and exit 56 | The ingress returned 407 requesting authentication—check the credentials or whether `listen_auth` follows `user:pass` |
| Occasional 502 / timeouts through this service | Dead residential egress nodes are normal; retry. For persistent failures, inspect the `err` details for `forward failed` in journalctl |
| Egress IP does not change with identity | First check whether `account_fp` differs in the audit log (routing side); if it is the same, the upstream provider is not routing by session parameters |
| `/metrics` returns 404 | Metrics is not enabled: enable it on the Basic Settings page, then access it while logged in as an administrator |
| Changed settings have no effect | Direct database edits require a restart; saving through the admin console/API takes effect hot |

## 10. Deployment Information Record (Fill In After Deployment)

After deployment, record the instance parameters in a **private operations channel** (do not commit them to the repository):

- Server IP / SSH method:
- Domain and certificate SAN coverage and validity period:
- Administrator password: exists only in the initial-start log (`journalctl -u mitmrouter | grep "Admin password:"`);
  change it immediately after logging in
- Ingress `listen_auth` credentials: view/change them on the admin console settings page
- Client CA distribution scope and method (which devices/containers have imported the root CA)
