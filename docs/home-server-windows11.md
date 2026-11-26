# Unseen Backup with Kopia: home Docker server, Cloudflare Tunnel admin, Windows 11 client

A worked example of [full-stack.md](full-stack.md) for one setup: the server
in Docker on a home machine, the admin GUI behind a Cloudflare Tunnel +
Access, and a Windows 11 PC as the first Kopia client. It covers the separate
admin port, the Kopia engine, agent self-update and the off-site copy to
OneDrive.

> **Tested so far:** the Kopia engine passed the repo's end-to-end test on
> Linux. **Nothing has been run on Windows yet** (VSS, paths, self-update
> restart), the full Docker stack has never been started as one piece, and
> the OneDrive copy has only been tested against a fake Microsoft server.
> Treat your Windows 11 PC as the first real test, and check a restore before
> you rely on it.

Placeholders (swap in your own):

| Placeholder | Meaning |
|---|---|
| `example.com` | your domain, DNS hosted on Cloudflare |
| `agents.example.com` | **agent** hostname: grey cloud, points at your home IP (kept current by DDNS) |
| `backup-admin.example.com` | **admin** hostname: through the Cloudflare Tunnel + Access |
| `192.168.1.50` | the home server's LAN IP |

---

## How the pieces fit

| Port | What | Who uses it | Exposure |
|---|---|---|---|
| **8443** | Agent API (no login page in this mode) | Windows agent | Router port forward, grey-cloud DNS |
| **51515** | Kopia repository server (same certificate as 8443) | Windows agent (Kopia jobs) | Router port forward |
| 8444 | Web GUI + admin API | You | **Tunnel + Access only.** Bound to 127.0.0.1 on the host |

```
 Windows 11 PC ──8443 + 51515 (pinned TLS)──▶ router ─▶ backup-server
                                                          │  Kopia repo on /data/kopia/repo (local disk)
 You (browser) ─▶ Cloudflare Access ─▶ Tunnel ─▶ cloudflared ─▶ backup-server:8444
                                                          │
                                       every 6 h: kopia sync-to ──rclone──▶ OneDrive (encrypted copy)
```

Agents pin the server's own certificate, so **agent ports can never go
through the tunnel**. The tunnel is only for you in a browser. With
`CB_ADMIN_LISTEN` set, no public port serves a login page.

---

## Step 1. Check you're not behind CGNAT

1. Router admin page → note the **WAN / Internet IP**.
2. On a home PC open `https://ifconfig.me`.
3. They must **match**. If the router's WAN IP differs, or starts `100.64`–`100.127`
   or another private range, you're behind CGNAT and port forwarding won't
   work. Ask your ISP for a public IPv4 (see the end of this guide for other options).

---

## Step 2. Prepare the home server

An always-on Linux box with Docker (Ubuntu Server assumed). Building the
image compiles the server, four agents **and Kopia for three platforms**. Give
it at least **4 GB RAM** and 10–20 minutes for the first build.

```bash
curl -fsSL https://get.docker.com | sudo sh
sudo usermod -aG docker $USER          # log out and back in

sudo mkdir -p /srv/unseen-backup/data   # put this on the big disk
sudo chown -R $USER /srv/unseen-backup
cd /srv/unseen-backup
git clone https://github.com/andycqos74/unseen-backup.git
```

The repo is private: use your GitHub username and a personal access token as
the password. Also set a **DHCP reservation** in your router so the server keeps
`192.168.1.50`.

---

## Step 3. Cloudflare: tunnel, Access, DDNS token

1. **Zero Trust → Networks → Tunnels → Create a tunnel** → Cloudflared →
   name `home-backup`. On the install page pick **Docker** and copy only
   the token after `--token`.
2. **Public hostname → Add**:
   - `backup-admin` . `example.com`
   - Service **HTTPS**, URL **`backup-server:8444`**
   - Additional settings → TLS → **No TLS Verify: ON**
3. **Access → Applications → Add → Self-hosted**: domain
   `backup-admin.example.com`, policy **Allow → Emails →** your email.
   Set this up before starting the stack.
4. **My Profile → API Tokens → Create Token → "Edit zone DNS"** for your zone.
   Copy it for DDNS.

---

## Step 4. Secrets and the compose stack

Generate the Kopia repository password and **save it in Vaultwarden now**.
If you lose it, every Kopia backup is unreadable, including the OneDrive copy.

```bash
openssl rand -hex 32
```

`/srv/unseen-backup/.env` (then `chmod 600 .env`):

```dotenv
# Agent-facing address: baked into every installer, so set it before enrolling
CB_SERVER_NAME=agents.example.com,192.168.1.50
CB_PUBLIC_URL=https://agents.example.com:8443

# Split admin GUI off the public port
CB_ADMIN_LISTEN=:8444
CB_ADMIN_URL=https://backup-admin.example.com

# Kopia engine (unset = off)
CB_KOPIA_PASSWORD=<the hex string from above>

# Cloudflare
TUNNEL_TOKEN=eyJhIjoi...
CLOUDFLARE_API_TOKEN=...
```

`/srv/unseen-backup/compose.yml`:

```yaml
services:
  backup-server:
    build:
      context: ./unseen-backup
      dockerfile: deploy/Dockerfile
      target: server
    image: unseen-backup/server
    container_name: backup-server
    restart: unless-stopped
    ports:
      - "8443:8443"              # agents
      - "51515:51515"            # Kopia repository server (agents)
      - "127.0.0.1:8444:8444"    # admin GUI: local only, the tunnel reaches it over the Docker network
    environment:
      CB_SERVER_NAME: ${CB_SERVER_NAME}
      CB_PUBLIC_URL: ${CB_PUBLIC_URL}
      CB_ADMIN_LISTEN: ${CB_ADMIN_LISTEN}
      CB_ADMIN_URL: ${CB_ADMIN_URL}
      CB_KOPIA_PASSWORD: ${CB_KOPIA_PASSWORD}
      TZ: Europe/London
    volumes:
      - ./data:/data             # database, TLS cert, Kopia repository, rclone config

  cloudflared:
    image: cloudflare/cloudflared:latest
    container_name: cloudflared
    restart: unless-stopped
    command: tunnel --no-autoupdate run --token ${TUNNEL_TOKEN}
    depends_on: [backup-server]

  ddns:
    image: favonia/cloudflare-ddns:latest
    container_name: cloudflare-ddns
    restart: unless-stopped
    network_mode: host
    environment:
      CLOUDFLARE_API_TOKEN: ${CLOUDFLARE_API_TOKEN}
      DOMAINS: agents.example.com
      PROXIED: "false"           # MUST stay grey-cloud: agents pin the server's own cert
      IP6_PROVIDER: none
```

This is the repo's `deploy/docker-compose.yml`, with these changes:
- a bind mount to `./data` instead of a named volume;
- MeshCentral left out (it's optional and you didn't ask for it). It can be
  added later with `--profile remote` and the `CB_MESH_*` variables;
- the tunnel and DDNS containers added.

Start and check:

```bash
cd /srv/unseen-backup
docker compose up -d --build
docker compose logs backup-server | grep -iE "fingerprint|kopia:|listen"
#   expect the cert fingerprint, and "kopia: repository … repository server on :51515"
docker compose logs cloudflared | tail      # "Registered tunnel connection"
docker compose logs ddns | tail             # shows the A record it set
```

---

## Step 5. Create the admin account, then open the router

1. Browse to `https://backup-admin.example.com` → Cloudflare Access PIN →
   the app's **setup** page. Create the admin account (long, unique password).
2. **Settings** → note the **TLS certificate fingerprint**. It must match the log.
3. Router → **Port forwarding**: TCP **8443** → `192.168.1.50:8443` and
   TCP **51515** → `192.168.1.50:51515`. Do **not** forward 8444.
4. Test from **outside** (phone on 4G, Wi-Fi off, or any external host):

```bash
curl -sk -o /dev/null -w '%{http_code}\n' https://agents.example.com:8443/login   # 404 = no GUI exposed (good)
openssl s_client -connect agents.example.com:51515 </dev/null 2>/dev/null \
  | openssl x509 -noout -fingerprint -sha256                                       # same fingerprint as 8443
```

---

## Step 6. Off-site copy to OneDrive (from the GUI)

The server mirrors the whole **encrypted** Kopia repository to OneDrive every
6 hours. OneDrive never sees your files unencrypted. The rclone it uses is
built into the server image, so nothing gets installed on the host.

**6a. Register an Azure app (once).** You need your own app because this is
self-hosted. In the GUI, the Settings page links to a step-by-step guide at
`https://backup-admin.example.com/static/storage-onedrive.html`. In short:

1. `https://entra.microsoft.com` → **App registrations → New registration**.
2. Supported account types: for personal OneDrive, **"Accounts in any
   organizational directory and personal Microsoft accounts"**.
3. Redirect URI, platform **Web**:
   `https://backup-admin.example.com/api/admin/storage/oauth/callback`.
   This comes from `CB_ADMIN_URL`, and the Settings page shows the exact value.
4. **Certificates & secrets → New client secret**. Copy the *value* and note
   its expiry date in your calendar.
5. Copy the **Application (client) ID**.

**6b. Connect.** GUI → **Settings → Off-site copy (Kopia)** → pick **OneDrive**,
paste the client ID and secret, choose a folder → **Connect and start
copying** → sign in to Microsoft and approve. Then use **Test connection** and
**Copy now**. The card shows the last result.

Don't change **Settings → Backup storage backend**. That's for the old
built-in engine. Leave it on Local.

**Recovering from OneDrive** if the server dies: on any machine with Kopia and
rclone, connect to the OneDrive copy with `CB_KOPIA_PASSWORD`, then run
`kopia snapshot list --all` and `kopia snapshot restore`. Snapshots are tagged
`agent:`, `job:` and `run:` so you can tell clients apart.

Also keep a copy of `/srv/unseen-backup/data/tls/` (the certificate and key).
If it's lost, the fingerprint changes and every agent must be re-enrolled.

---

## Step 7. Install the agent on the Windows 11 PC

1. On the PC open `https://backup-admin.example.com` → **Clients → Enroll new
   client** → **Windows (64-bit)** → `backup-agent-installer.exe`. The agent
   address, a one-time token (24 h) and the fingerprint are embedded. It
   must be downloaded from *this* server: an installer from an older build
   has no Kopia.
2. **Move it to a permanent folder first.** The service runs the exe from
   wherever it sits at install time. Self-update replaces it in place, so
   Downloads is a bad home. In an **Administrator PowerShell**:

   ```powershell
   New-Item -ItemType Directory -Force "C:\Program Files\UnseenBackup" | Out-Null
   Move-Item "$env:USERPROFILE\Downloads\backup-agent-installer.exe" "C:\Program Files\UnseenBackup\"
   Unblock-File "C:\Program Files\UnseenBackup\backup-agent-installer.exe"   # unsigned exe; skips SmartScreen
   cd "C:\Program Files\UnseenBackup"
   .\backup-agent-installer.exe install
   ```

   This enrolls the PC and starts the **`CentralBackupAgent`** service. On
   first connect the agent gets its own Kopia login from the server and
   downloads the Kopia binary from the server, with its SHA-256 checked.
   MeshAgent is skipped because remote access isn't configured. State lives in
   `C:\ProgramData\BackupAgent\`.

3. **Same home network as the server?** The PC will look up `agents.example.com`
   and get your *public* IP. Many routers can't loop that back. Pin it to the
   LAN IP instead. This covers both 8443 and 51515, and it's safe because the
   agent checks the certificate fingerprint, not the IP:

   ```powershell
   Add-Content "$env:WINDIR\System32\drivers\etc\hosts" "`n192.168.1.50  agents.example.com"
   ```

   Leave this out on machines outside your home, and on a laptop that leaves
   home unless your router supports NAT loopback.

4. Check:

   ```powershell
   Get-Service CentralBackupAgent                         # Running
   Test-NetConnection agents.example.com -Port 8443       # TcpTestSucceeded : True
   Test-NetConnection agents.example.com -Port 51515      # TcpTestSucceeded : True
   ```

   The PC shows under **Clients** within seconds.

---

## Step 8. First Kopia backup and a restore test

1. GUI → the client → **New job**. **Backup engine: Kopia** (the default when
   Kopia is on). Path e.g. `C:\Users\<you>\Documents`, schedule `0 2 * * *`,
   keep last 30. Excludes and retention are applied centrally as Kopia policy.
2. **Back up now**, and watch the live log.
3. Open the snapshot → **Browse / restore** → restore a file to
   `C:\RestoreTest` and open it. Also try one **Download**.
4. Optional VSS test: back up a folder with a file held open (e.g. an open
   Outlook PST). With Kopia it should be captured, not skipped. This is the
   first Windows test of that code, so tell me what happens.
5. GUI → **Settings → Off-site copy → Copy now**, and confirm it reports success.

What's different from the old engine:
- Clients are **append-only**: the PC can add snapshots but can't delete
  them. Retention runs on the server.
- Every Kopia snapshot is incremental and complete, so "full every Nth run"
  doesn't apply.
- Agents **self-update** to the server's build when you redeploy the image.
  Turn that off with `CB_AGENT_AUTOUPDATE=false` on the server.

---

## When your home IP changes

The DDNS container updates `agents.example.com` within minutes. The agent
drops its dead connection after 90 s of silence, then reconnects with backoff
and looks up the name again. It normally recovers within about 5 minutes,
with nothing for you to do. A backup scheduled in that gap is queued and runs
on reconnect. The admin tunnel is unaffected.

---

## Updating later

```bash
cd /srv/unseen-backup/unseen-backup && git pull
cd .. && docker compose up -d --build
```

Agents update themselves to the new build. When the PRs are merged, switch
the clone to `main` (`git checkout main && git pull`) and rebuild.

---

## Troubleshooting

| Symptom | Fix |
|---|---|
| `server certificate fingerprint mismatch` on the agent | `agents.example.com` is orange-clouded. Set it to **DNS only**. |
| Tunnel 502 | Service must be **HTTPS → `backup-server:8444`** with No TLS Verify. Check `CB_ADMIN_LISTEN=:8444` is set. |
| GUI loads on `:8443` | `CB_ADMIN_LISTEN` isn't set, so the login page is public. Fix `.env` and run `docker compose up -d`. |
| Kopia job fails to connect | Port **51515** isn't forwarded, or the hosts entry is missing on a home PC. |
| Server won't start, Kopia error | Check `CB_KOPIA_PASSWORD`. Changing it after the repo exists will not open the old repo. |
| OneDrive `invalid_request` / redirect mismatch | Azure redirect URI must exactly match the one shown in Settings (from `CB_ADMIN_URL`). |
| Off-site copy suddenly failing | The Azure client secret expired, or consent was revoked. Make a new secret, then reconnect on the card. |
| Token invalid | Installers are single-use and expire after 24 h. Download a fresh one. |

Agent logs: **Event Viewer → Windows Logs → Application**, source
`CentralBackupAgent`.

---

## Known gaps

1. **Untested on Windows**, as covered at the top.
2. **The installer doesn't copy itself to Program Files**, so Step 7.2 does it by hand.
3. **The app's own login has no 2FA.** That's acceptable now that the GUI is
   only reachable through Cloudflare Access.
4. **The primary Kopia repository is local only.** The OneDrive copy is the
   off-site mirror, so OneDrive can't be the primary store.
5. **Browsing or zip-downloading a very large snapshot can time out.**
   Restore to the client still works.
6. **32-bit and Windows 7 clients** keep the old built-in engine.
7. **The installer isn't code-signed.**

## If you're behind CGNAT

The tunnel still works for admin, but agents need a direct path. Options:
- get a public IPv4 from your ISP;
- use WireGuard/Tailscale between clients and server, with `CB_PUBLIC_URL`
  set to the VPN address before enrolling;
- use a small VPS that forwards TCP 8443 and 51515 to home over WireGuard, as
  plain TCP passthrough with no TLS termination.
