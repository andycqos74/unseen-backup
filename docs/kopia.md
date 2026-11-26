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
- **Policies are central.** Before each run (and again after it, for the
  path exactly as Kopia recorded it) the server writes the job's excludes
  and retention as Kopia policy: *Keep last N* → keep-latest N; *also keep
  newer than N days* → one snapshot per hour and per day for N days (Kopia
  has no plain age rule, so runs more frequent than hourly are thinned to
  one an hour beyond the last N); neither set → keep everything. The global
  policy keeps everything, so a path that somehow doesn't match its job's
  policy is never pruned by Kopia's defaults. Expiry runs after each run and
  daily; the server is the repository's maintenance owner
  (`controller@backup-server`), so freed space is reclaimed by Kopia's
  scheduled maintenance.
- **The server trusts the repository, not the client.** When an agent
  records a run, the server reads every snapshot back from the repository
  and refuses ones that aren't that client's; sizes and paths shown in the
  GUI come from the repository.
- The **kopia binary** for each client is built into the server image
  (Linux x64/ARM64, Windows x64) and fetched by the agent from the server
  over its pinned connection, verified by SHA-256 — still one installer per
  client. 32-bit Windows clients (including Windows 7) have no Kopia build
  and keep the built-in engine.
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

## Off-site copy (3-2-1)

Set `CB_KOPIA_SYNC_TO` and the server mirrors the repository there with
`kopia repository sync-to … --delete` every `CB_KOPIA_SYNC_INTERVAL`
(default `6h`; first copy a few minutes after start). The copy is the same
encrypted repository — the storage provider never sees plaintext — and
*Settings → Off-site copy* shows the last result.

### OneDrive or Google Drive (from the GUI)

*Settings → Off-site copy (Kopia)*: pick OneDrive or Google Drive and a
folder, then **Connect and start copying**. It uses the same OAuth app and
redirect URI as the storage backend section (leave client ID/secret empty
to reuse those), but it is a separate setting: the built-in engine's storage
is not changed. **Test connection**, **Copy now** and **Disconnect** are on
the same card, with the last result.

How it works: at connect the server writes `/data/kopia/rclone.conf`
(0600) with the account's token, once — from then on rclone owns the token
(Microsoft rotates refresh tokens; rclone saves the new one). One long-lived
`rclone serve webdav` on loopback (`127.0.0.1:51516`) serves the cloud
folder, and `kopia repository sync-to webdav` copies to it. Uploads are not
cached locally, so a finished copy is really in the cloud.

Google Drive uses the `drive.file` scope: the app (and so the copy) can only
see files it created itself. OneDrive needs `Files.ReadWrite` (already
requested) and works for personal and business drives.

If the connection breaks (password change, revoked consent), the card shows
the error; connect again to issue a new token.

### S3-compatible storage (by environment)

`CB_KOPIA_SYNC_TO` takes precedence over a GUI destination. Any
S3-compatible store (Backblaze B2, Cloudflare R2, Wasabi, AWS):

```bash
CB_KOPIA_SYNC_TO="s3 --bucket=my-backups --endpoint=s3.eu-central-003.backblazeb2.com --prefix=unseen/"
AWS_ACCESS_KEY_ID=…          # Vaultwarden; never put keys in CB_KOPIA_SYNC_TO
AWS_SECRET_ACCESS_KEY=…
```

Other `sync-to` targets (`filesystem --path=/mnt/nas/kopia`, `sftp …`,
`b2 …`, `azure …`, `gcs …`) work the same way; keep secrets in the
environment variables Kopia reads for them.

**Disaster recovery**: on any machine with Kopia, `kopia repository connect
s3 --bucket=… --endpoint=… --prefix=…` with `CB_KOPIA_PASSWORD`, then
`kopia snapshot list --all` and `kopia snapshot restore`. Snapshots carry
`agent:`, `job:` and `run:` tags to tell clients apart.

## Notes and limits

- **The primary repository is on the server's disk.** The off-site copy
  above covers losing the server. Keeping the *primary* repository in
  OneDrive/Google Drive/S3 (to save server disk) isn't wired up: it would
  also make browsing and restores slower, since every read goes to the cloud.
- The OneDrive/Google Drive token handoff to rclone was tested against a fake
  OAuth/Graph server and the copy against a local rclone remote; it has not
  been run against a real Microsoft or Google account here.
- **Existing clients need the new agent.** Agents don't self-update; a client
  gets Kopia (and the MeshAgent bootstrapper) after reinstalling the
  agent from the enroll dialog.
- **Browsing or zip-downloading a very large snapshot can time out.** The
  server lists a Kopia snapshot by walking it with one `kopia show` per
  directory (cached for the last 8 snapshots) under a 10-minute limit; a
  tree with tens of thousands of folders may not finish. Restore to the
  client is unaffected (Kopia restores directly).
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
snapshot, the off-site copy (to a filesystem target) opening as a complete
repository with the snapshot, and deletion from the GUI. Windows (VSS, paths) is covered by build
and code review only.
