# Remote access (MeshCentral)

Remote desktop, terminal and file access to clients come from a self-hosted
[MeshCentral](https://github.com/Ylianst/MeshCentral) running in the same
Docker stack. The backup GUI adds **Desktop / Terminal / Files** buttons to
each client page; a click signs you in to MeshCentral and opens that device's
tab in a new browser tab. Remote sessions run between your browser, MeshCentral
and MeshAgent — they never pass through the backup server.

```
 admin browser ──(Cloudflare Tunnel + Access)──▶ backup GUI  :8444  (CB_ADMIN_LISTEN)
       │                                          │  mints a 2-minute, single-use
       │                                          │  MeshCentral login token per click
       └──(Cloudflare Tunnel + Access)──▶ MeshCentral UI :443   (not published)
                                                   ▲
 MeshAgent on each client ──(direct, TLS)──▶ MeshCentral agent port :4443 (published)
 backup agent on each client ──(direct)────▶ backup agent port      :8443 (published)
```

## How it fits together

- **Two published ports, no login pages on either.** `8443` is the backup
  agent listener (see [deployment §3](deployment.md#admin-gui-behind-a-cloudflare-tunnel-separate-admin-listener));
  `4443` is MeshCentral's agent-only listener (`settings.agentPort`), which
  serves MeshAgent connections, the session relay and agent downloads, and
  nothing else. Both GUIs sit behind the tunnel.
- **Login hand-off.** MeshCentral's `LoginCookieEncryptionKey` is set from
  `MESH_LOGIN_KEY`; the backup server holds the same value as
  `CB_MESH_LOGIN_KEY` and uses it to mint tokens in MeshCentral's own cookie
  format (`{u, a:3}` with `expire: 2` and a `once` nonce — MeshCentral rejects
  the token after 2 minutes or after its first use). The token is created on
  the click, sent only in a `302` redirect (`Cache-Control: no-store`,
  `Referrer-Policy: no-referrer`), and never appears in page HTML. After it
  is used, the browser has an ordinary MeshCentral session.
- **Device mapping.** The backup server lists MeshCentral devices through its
  control API (`control.ashx`) and links a client to the device whose
  agent-reported computer name matches the client's hostname. When nothing or
  several devices match, the client page offers a picker; *change* unlinks.
- **The key is a master key.** Anyone holding `MESH_LOGIN_KEY` can sign in to
  MeshCentral as any user. Generate it with `openssl rand -hex 80`, store it
  in Vaultwarden, and never put it in the repository.

## One installer: the bootstrapper

Clients need only the backup-agent installer. When remote access is
configured, the backup agent installs MeshAgent itself, about 20 seconds after
it starts:

1. It asks the backup server for MeshAgent (`GET /api/agent/meshagent`) over
   its existing, certificate-pinned connection — clients never have to trust
   MeshCentral's self-signed certificate for this.
2. The server fetches the right build from MeshCentral (Windows x64/x86/ARM64,
   Linux x64/ARM64/x86) with the device group's settings embedded, and
   records when it handed it out.
3. The agent runs it silently (`-fullinstall` on Windows, `-install` on
   Linux), checks the MeshAgent service now exists, and writes
   `meshagent-installed` in its state directory.
4. The client page links the new MeshCentral device **exactly**: it is the
   device with the client's hostname whose first connection followed that
   download, so other machines with the same name (tills, cloned PCs) don't
   confuse it. Hostname matching remains the fallback for MeshAgents installed
   some other way, and a device picked by hand is never replaced.

Behaviour worth knowing:

- **Installed once.** The marker file stops a reinstall after someone removes
  MeshAgent on purpose. Delete `meshagent-installed` (in
  `/var/lib/backup-agent` or `C:\ProgramData\BackupAgent`) to have it
  installed again.
- **Opt out** fleet-wide with `CB_MESH_AUTOINSTALL=false` on the server, or on
  one machine by setting `CB_NO_REMOTE_ACCESS=1` in the agent's environment.
- **Already has MeshAgent?** Nothing is installed; hostname matching (or the
  picker) links it.
- **Docker-host agent** (the containerized agent) skips it: MeshAgent would
  land in the container, not on the host. Install MeshAgent on the host with
  the Linux command from the enroll dialog.
- Failures are logged by the agent (`remote access: …`) and retried every 30
  minutes.
- The installer also carries the client's ID as MeshAgent's *agent tag*,
  and the server links by tag when MeshCentral reports one. MeshAgent 1.3.1
  was not seen reporting it in testing, so the install-time match is what
  links in practice.

## Setup

DNS / tunnel:

| Name | Type | Points at |
|---|---|---|
| `agents.example.com` | `A`, **grey cloud** | server IP — ports 8443 and 4443 |
| `backup-admin.example.com` | Tunnel public hostname + Access | `https://backup-server:8444`, No TLS Verify |
| `mesh.example.com` | Tunnel public hostname + Access | `https://meshcentral:443`, No TLS Verify |

**How cloudflared reaches the two UIs.** The service names above
(`backup-server`, `meshcentral`) only resolve if cloudflared is on the same
Docker network as these containers. Either add your shared tunnel network to
both services (e.g. an external network `tunnel` that cloudflared is also on:
`networks: [default, tunnel]` plus `networks: {tunnel: {external: true}}`),
or, with cloudflared running on the host, use `https://127.0.0.1:8444` and
`https://127.0.0.1:8445` — the compose files publish both on localhost only.

**Egress.** MeshCentral fetches agent binaries on its first start, so its
container needs outbound internet access (don't put it only on an
`internal: true` network).

**One Access session for both hostnames.** The MeshCentral token lives for 2
minutes. If clicking *Desktop* first triggers a Cloudflare Access sign-in for
`mesh.example.com` that takes longer, you land on MeshCentral's own login
page — just click the button again. Putting both admin hostnames in the same
Access application avoids the second sign-in.

Open TCP **4443** (as well as 8443) in both the Fasthosts cloud-panel firewall
and ufw. Docker-published ports bypass ufw, so the cloud firewall is the one
that actually filters.

Variables (`.env` next to the compose file, or Portainer's stack variables):

```bash
# MeshCentral container
MESH_AGENT_HOST=agents.example.com
MESH_AGENT_PORT=4443
MESH_UI_HOST=mesh.example.com
MESH_LOGIN_KEY=<openssl rand -hex 80>          # Vaultwarden

# backup server
CB_ADMIN_LISTEN=:8444
CB_PUBLIC_URL=https://agents.example.com:8443
CB_ADMIN_URL=https://backup-admin.example.com
CB_MESH_URL=https://mesh.example.com
CB_MESH_API_URL=https://meshcentral:443
CB_MESH_AGENT_URL=https://agents.example.com:4443
CB_MESH_LOGIN_KEY=<same value as MESH_LOGIN_KEY>
CB_MESH_USER=admin                              # your MeshCentral account name
CB_MESH_GROUP=                                  # only needed with several device groups
```

Start (compose):

```bash
docker compose -f deploy/docker-compose.yml --profile remote up -d
```

Then:

1. Open `https://mesh.example.com` and **create the first account** — with
   new accounts disabled, MeshCentral still allows the first one and makes it
   site administrator. Use the name you put in `CB_MESH_USER`. Turn on 2FA.
2. Create a **device group** (*My Devices → Add Device Group*,
   "Manage using a software agent").
3. Enroll clients as usual (*Clients → Enroll new client*, one installer).
   Each backup agent installs MeshAgent into that group by itself.
4. Open a client's page and use **Desktop / Terminal / Files**.

## Verify

```bash
# agent port: downloads work, no UI
curl -sk -o /dev/null -w '%{http_code}\n' https://agents.example.com:4443/      # 404
docker logs meshcentral 2>&1 | grep -i "agent-only server running"             # on port 4443

# MeshCentral UI published on localhost only
docker port meshcentral                                                         # 4443 on 0.0.0.0, 443 on 127.0.0.1:8445 only
```

From the backup server container, the API should answer (look for
`MeshCentral remote access enabled` in its log; a client page showing
"MeshCentral unavailable: … refused the API session (noauth)" means
`CB_MESH_LOGIN_KEY` ≠ `MESH_LOGIN_KEY` or `CB_MESH_USER` doesn't exist).

## Notes and limits

- **`MESH_AGENT_HOST` must contain a dot.** MeshCentral switches to LAN-only
  mode (agents look for the server by multicast, not by URL) when its
  certificate name has no dot, e.g. `localhost`.
- **Origin check.** MeshCentral rejects browser websockets from hostnames it
  doesn't know ("Invalid origin in HTTP request"). The stack sets
  `allowedOrigin` to `MESH_UI_HOST`; if you reach the UI some other way (an
  SSH tunnel to `127.0.0.1:8445`), add that hostname too or expect the error.
- **MeshCentral's own certificate.** MeshAgents verify the server by the
  hashes embedded in their installer, so the self-signed certificate is fine.
  Don't enable `tlsOffload` or `certUrl`, and don't put the agent port behind
  any TLS-terminating proxy.
- **API certificate.** `CB_MESH_API_URL` is on the internal Docker network,
  and its certificate isn't verified unless you set
  `CB_MESH_API_FINGERPRINT` (SHA-256 of MeshCentral's web certificate, hex).
- **Antivirus.** MeshAgent is a remote-access tool and some products flag it;
  allow-list it in managed AV.
- **Windows 7 clients** are supported by MeshAgent.
- `config.json` is regenerated from the variables on every container start.
  Change the variables, not the file. Data, accounts and device groups live in
  the `meshcentral-data` volume and survive restarts.

## Tested

Run against MeshCentral from source (master at `7c17f6d`, version 1.3.1 plus
later fixes); every behaviour relied on here was then re-checked in the
`1.3.1` release tag the stack pins, and the `ghcr.io/ylianst/meshcentral:1.3.1`
image was confirmed to exist. Covered: token
interop both ways with MeshCentral's `encodeCookie`/`decodeCookie`; single-use
rejection on reuse; control API listing groups and devices; wrong-key refusal;
Windows and Linux MeshAgent downloads from the agent-only port (the UI returns
404 there) — the enroll dialog's Windows *button* uses the UI origin instead,
so the admin's browser sees the tunnel's certificate; a real Linux MeshAgent connecting through the agent port; the
client page auto-linking by hostname; and, in a browser, the
Desktop/Terminal/Files buttons landing signed in on the right device's tab,
with a live terminal session relayed through the agent port. (A remote
*desktop* picture could not be shown: the test machine has no display.) The Docker stack
definition itself was not run (no Docker in the build environment) — its
config script was executed with test values.

The bootstrapper was tested end to end on Linux: the real backup agent
fetched MeshAgent through the backup server (which pulled it from MeshCentral),
installed it, detected the service, and the client page linked the new device
exactly while three other devices had the same hostname. The Windows path
(service detection, `-fullinstall`) is covered by build and code review only.
