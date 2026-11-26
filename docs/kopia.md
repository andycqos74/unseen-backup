# Kopia backup engine

Jobs can use [Kopia](https://kopia.io) as their backup engine instead of the
built-in one. The central server keeps all the control it has today —
scheduling, run-now, live run log, restore to client, browse and download in
the GUI, job sync with `agent.yaml` — and Kopia does the storage:

- **content-defined chunking**: a small change to a big file (PST, SQL
  `.bak`, VM image) stores only the changed chunks, not the whole file;
- **zstd compression** and **packing** of small files into large objects;
- an **encrypted, self-describing repository** (recoverable without the
  server's database);
- **Windows shadow copies (VSS)**: open and locked files are captured
  consistently instead of skipped.

## How it works

```
 agent ──(control channel, :8443)──▶ backup server ──▶ Kopia repository (/data/kopia/repo)
   │                                   │ runs `kopia server` (child process)
   └──(Kopia API, :51515, same cert)──▶┘ sets policies, applies retention, browses
```

- The backup server **owns the repository** (filesystem, in its data volume)
  and runs `kopia server` itself on `:51515`, with the **same TLS
  certificate** as the agent port — agents pin the fingerprint they already
  trust.
- Each client is its own Kopia user, `agent@<client id>`, provisioned
  automatically when an agent that supports Kopia connects. Its password is
  sent over the control channel and stored in the agent's state directory —
  never in an installer.
- **Clients are append-only.** Server ACLs let a client create snapshots and
  read its own policy, but not delete snapshots or change policies. A
  compromised client cannot destroy its own backups. Retention runs on the
  server.
- **Policies are central.** Before each run the server writes the job's
  excludes and retention as Kopia policy for each path (*Keep last N* →
  keep-latest, *newer than N days* → keep-daily; neither set → keep
  everything). Expiry runs after each run and daily.
- The **kopia binary** for each client is built into the server image
  (linux, windows, macOS × amd64/arm64) and fetched by the agent from the
  server over its pinned connection, verified by SHA-256 — still one
  installer per client. Windows 7 / 32-bit clients have no Kopia build and
  keep the built-in engine.
- Snapshots are tagged with agent, job and run IDs, so they can be traced
  from the repository alone.

In the GUI nothing changes except a **Backup engine** choice in the job
editor (Kopia is the default when enabled). Kopia snapshots appear in the
same list and support browse, single-file and zip download, restore to the
original location or another folder (all or selected files), and delete.

## Enable it

```bash
CB_KOPIA_PASSWORD=<openssl rand -hex 32>   # repository password — store in Vaultwarden
CB_KOPIA_URL=https://agents.example.com:51515   # optional; default is the agent host + :51515
```

and publish **TCP 51515** (both compose files do; open it in the Fasthosts
panel and ufw like 8443). Agents reach it directly, never through the
Cloudflare Tunnel (they pin the certificate).

On first start the server creates the repository at `/data/kopia/repo` and
logs `kopia: repository … repository server on :51515`. Then create a job
with engine *Kopia*, or switch an existing job (its old snapshots stay
restorable under the built-in engine and age out by retention).

Other settings: `CB_KOPIA_LISTEN` (default `:51515`), `CB_KOPIA_REPO`
(default `/data/kopia/repo`), `CB_KOPIA_BIN` (default `kopia` on `PATH`).

> **The repository password cannot be recovered.** Lose `CB_KOPIA_PASSWORD`
> and every Kopia backup is unreadable. Keep it in Vaultwarden.

## Notes and limits

- **Repository on local disk only (for now).** The repository lives in the
  server's data volume. Off-site copies (`kopia repository sync-to` to
  B2/R2/S3) and S3-backed repositories are not wired into the GUI yet.
- **Existing clients need the new agent.** Agents don't self-update; a client
  gets Kopia (and the MeshAgent bootstrapper) after reinstalling the
  agent from the enroll dialog.
- **Browsing a big snapshot** walks it with `kopia show` once and caches the
  listing (last 8 snapshots); the first open of a very large snapshot is slow.
- *Full every Nth run* doesn't apply: every Kopia snapshot is incremental
  and logically complete.
- Run statistics show files and size; Kopia doesn't report uploaded bytes per
  run.
- The server stores Kopia user passwords in its database (like other
  secrets there) and passes them to `kopia server user add` as an argument
  inside its own container.

## Tested

`scripts/e2e-test.sh` with `KOPIA_BIN=/path/to/kopia` (Kopia v0.23.1, also in
split-listener mode) runs a real server, agent and Kopia: provisioning, a
backup with excludes applied as policy, browse, single-file and zip download,
full and selected-folder restore, the client being denied deletion of its own
snapshot, and deletion from the GUI. Windows (VSS, paths) is covered by build
and code review only.
