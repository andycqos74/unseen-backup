# Full stack: backup + Kopia + remote access

One Docker stack gives central, web-managed backup and remote access for LAN
and WAN machines. This page ties the pieces together; details are in
[deployment.md](deployment.md), [kopia.md](kopia.md) and
[remote-access.md](remote-access.md).

## What runs where

| Port | Service | Who connects | Exposure |
|---|---|---|---|
| 8443 | backup agent API (`CB_LISTEN`) | backup agents | **public**, grey-cloud DNS |
| 51515 | Kopia repository server | backup agents (Kopia jobs) | **public**, same certificate as 8443 |
| 4443 | MeshCentral agent port | MeshAgents | **public** |
| 8444 | backup GUI + admin API (`CB_ADMIN_LISTEN`) | you | Cloudflare Tunnel + Access only (`127.0.0.1` on the host) |
| 443 (in container) | MeshCentral UI | you | Cloudflare Tunnel + Access only (`127.0.0.1:8445` on the host) |

No public port serves a login page. Agents pin certificates, so their ports
are never behind the tunnel.

## Variables

```bash
# backup server
CB_SERVER_NAME=agents.example.com
CB_PUBLIC_URL=https://agents.example.com:8443
CB_ADMIN_LISTEN=:8444
CB_ADMIN_URL=https://backup-admin.example.com
CB_KOPIA_PASSWORD=<openssl rand -hex 32>          # Vaultwarden — unrecoverable if lost
CB_KOPIA_SYNC_TO="s3 --bucket=… --endpoint=… --prefix=unseen/"   # optional off-site copy
AWS_ACCESS_KEY_ID=… ; AWS_SECRET_ACCESS_KEY=…      # for the off-site copy (Vaultwarden)
CB_MESH_URL=https://mesh.example.com
CB_MESH_API_URL=https://meshcentral:443
CB_MESH_AGENT_URL=https://agents.example.com:4443
CB_MESH_LOGIN_KEY=<openssl rand -hex 80>           # Vaultwarden; same value as MESH_LOGIN_KEY
CB_MESH_USER=admin

# MeshCentral
MESH_AGENT_HOST=agents.example.com
MESH_UI_HOST=mesh.example.com
MESH_LOGIN_KEY=<same as CB_MESH_LOGIN_KEY>
```

## Bring-up checklist

1. DNS: `agents.example.com` → server IP, **grey cloud**. Tunnel public
   hostnames `backup-admin.example.com` → `https://backup-server:8444` and
   `mesh.example.com` → `https://meshcentral:443`, both with *No TLS Verify*
   and a Cloudflare Access policy. Put cloudflared on the stack's network
   (or use `127.0.0.1:8444` / `127.0.0.1:8445` from a host cloudflared).
2. Firewall: open TCP 8443, 51515 and 4443 in the Fasthosts panel **and**
   ufw (Docker publishes bypass ufw, so the panel is what really filters).
3. Start: Portainer stack (`deploy/portainer-stack.yml`) or
   `docker compose -f deploy/docker-compose.yml --profile remote up -d`.
4. Open `https://backup-admin.example.com`, create the admin account.
   Open `https://mesh.example.com`, create the first MeshCentral account
   (named as `CB_MESH_USER`), turn on 2FA, create one device group.
5. Enroll a client: *Clients → Enroll new client* → download the installer
   → run it as administrator. It enrolls, installs the backup service,
   connects Kopia, and installs MeshAgent by itself.
6. On the client's page: create a job (engine *Kopia*), *Back up now*, then
   try *Desktop / Terminal / Files*.

## Verify

```bash
curl -sk -o /dev/null -w '%{http_code}\n' https://agents.example.com:8443/login   # 404 (no GUI)
curl -sk -o /dev/null -w '%{http_code}\n' https://agents.example.com:4443/        # 404 (no MeshCentral UI)
openssl s_client -connect agents.example.com:51515 </dev/null 2>/dev/null \
  | openssl x509 -noout -fingerprint -sha256                                       # same as port 8443
docker logs backup-server 2>&1 | grep -E "kopia:|MeshCentral remote access"
```

## After that, it maintains itself

- Agents **self-update** to the server's build when you deploy a new image
  ([operations.md](operations.md#agent-self-update)).
- Kopia retention and expiry run on the server; clients can't delete their
  own backups.
- The off-site copy runs every 6 hours; *Settings* shows its last result.

## Not yet covered

- Nothing has been run on **Windows** in testing: the MeshAgent
  bootstrapper, self-update restart, VSS and Windows paths are covered by
  build and code review only. Try one Windows machine before rolling out.
- The full Docker stack itself hasn't been started in testing (each part was
  run on its own).
- Kopia browse/zip of very large snapshots can time out; restore is
  unaffected.
- Not done: S3 as the *primary* Kopia repository, Headscale/Tailscale,
  code-signed installers.
