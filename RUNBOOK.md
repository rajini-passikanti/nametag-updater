# Update Operations Runbook

Commands assume the repository root, release content in `releases/`, and the signing key in `keys/k1.key`. Shell examples use Bash; the Go CLIs also run on Windows. Use the [README](README.md) for initial setup and the [PRD](PRD.md) for behavior and security boundaries.

## Operating rules

- Publish a higher version to fix a release. Server-offered downgrades are rejected; local recovery may restore the previous executable after an unconfirmed update.
- A version identifies one complete, immutable artifact set across all channels. Promote the same build; use a new version if any bytes or platform artifacts change.
- Keep release-signing private keys off the update server and out of Git. The example file-based signer is intended to run on a protected signing machine.
- Publish and refresh share `releases/.publish.lock`. Retry a busy command after the active operation completes. Pause publishing before manual manifest replacement; do not delete lock files while processes are running.
- The server's `/status` endpoint reports offered releases. Client adoption and health must be assessed from client logs or an external telemetry integration.
- Crash rollback needs a user or service manager to restart the client. No supervisor or automatic fleet-wide rollout halt is supplied.

## Routine release and promotion

1. Generate a signing key once with `make keygen`, if one does not already exist.
2. Build and publish to beta:

   ```sh
   make release VERSION=1.4.0 CHANNEL=beta
   ```

   Beta clients use `--channel beta`.
3. Inspect client logs for `update installed`, a new-version heartbeat, and `update confirmed healthy`. Investigate any rejection, failed handover, or rollback before promotion. A local rollback does not stop server publication automatically.
4. Promote the identical artifact set to stable:

   ```sh
   go run ./cmd/release publish --version 1.4.0 \
     --dist dist/1.4.0 --channel stable
   ```

5. Inspect `GET /status` on the deployed update server. The `stable` channel should show 1.4.0 with the expected platforms and expiry. Local example:

   ```sh
   curl -fsS http://127.0.0.1:8080/status
   ```

Use HTTPS for non-loopback deployment. Public release content can be cached behind a CDN; manifests must retain `no-store` behavior and the application's endpoint routing.

## Halt a bad rollout and fix forward

**Trigger:** repeated client failures, unexpected recovery events, or a bad release discovered during validation.

1. Pause publish/refresh jobs and ensure no publication process is active. Restore the last good signed manifest using a same-directory temporary file and rename:

   ```sh
   cp releases/manifests/history/stable/1.3.0.json \
     releases/manifests/.stable.halt.tmp
   mv -f releases/manifests/.stable.halt.tmp releases/manifests/stable.json
   go run ./cmd/release refresh --channel stable
   ```

   Refresh renews expiry after checking the signature and artifacts. Clients still on 1.3.0 stop being offered 1.4.0. Clients already on 1.4.0 do not accept a server-offered downgrade. After two unconfirmed boots, a later external restart that reaches updater startup attempts local rollback.
2. Publish a fix with a new version number:

   ```sh
   make build VERSION=1.4.1
   go run ./cmd/release publish --version 1.4.1 \
     --dist dist/1.4.1 --revoke 1.4.0
   ```

   Revocation overrides application-requested deferral. It does not bypass pending-update safety or restore a broken installation that cannot start.
3. If future clients must meet a security floor, add `--min-version 1.4.1` to subsequent releases. Verify new-version health before resuming normal publication.

If the server's release files may have been modified by an attacker, restore artifacts from trusted build output and investigate before signing or refreshing anything.

## Diagnose clients that are not updating

| Log or observation | Interpretation | Action |
| --- | --- | --- |
| `manifest has expired` | Manifest expiry has passed. | Verify the channel and run `release refresh`. Check client clock accuracy. |
| `release is revoked` | The offered candidate is listed as revoked. | Publish a valid higher version. |
| `no artifact for this platform` | The manifest lacks the client's OS/architecture. | Publish a new version with the required target. |
| `signature is invalid` or `unknown key` | The manifest does not authenticate against this client's embedded key ring. | Investigate publication and key compatibility; treat unexpected failures as a potential incident. |
| `sha256 does not match` or `size does not match` | Downloaded bytes differ from the signed artifact. | Investigate corruption or tampering; compare trusted build output and served files. |
| `self-test of ... failed` | The candidate failed preflight. | It is skipped for that process's lifetime. Publish a corrected higher version. |
| `replace executable` | Backup, permissions, rename, or storage operation failed. | Inspect the full error and install permissions; check transient Windows file locks and storage health. |
| `fetch manifest` | The update endpoint is unavailable or rejected the request. | Check endpoint availability, configuration, and server logs. |
| `update available but deferred by application` | The optional application hook requested deferral. | Expected for a busy application. Minimum-version/revocation policy can override the hook. |
| No install while `pending` exists | Health confirmation or recovery is still outstanding. | Preserve the fallback; resolve the pending state before further installation. |
| `replacement committed with durability error` | Replacement succeeded but directory synchronization failed. | Check storage health. Keep pending recovery state; the app proceeds to handover. |
| `handover failed; resuming workload` | Candidate launch failed. | Successful recovery blocks the candidate. If recovery also failed, repair the installation and restart. |
| `automatic updates disabled; repair and restart required` | Startup or handover recovery could not safely finish. | Preserve state and backups, repair the installation, then restart. The current workload remains running. |
| Release appears in `blocked` | Local rollback or handover recovery rejected it. | Publish a higher corrected version. |
| Another app invocation waits | `app.lock` is held by the running instance. | Expected. Stop the existing instance through its normal process/service controls if replacement is intended. |

A release at or below the running version or high-water mark is treated as up to date and logged at debug level. Default app logging is informational.

## Inspect local recovery state

The default file is `.updater/state.json` beside the executable:

```json
{
  "install_id": "8472d155ffdc53ae7105511610908459",
  "high_water": "1.3.0",
  "blocked": ["1.4.0"],
  "pending": {
    "version": "1.4.1",
    "from": "1.3.0",
    "boots": 1,
    "sha256": "<candidate SHA-256>",
    "from_sha256": "<previous executable SHA-256>"
  }
}
```

- `high_water` is the newest version confirmed healthy; server-offered releases at or below it are rejected.
- `pending` records an update awaiting confirmation or recovery. Its digests distinguish interrupted installs from stale processes and validate the backup. The example digest values above are placeholders.
- `boots` counts unconfirmed startup attempts. After two such boots, the next external launch attempts rollback.
- `blocked` persists versions rejected after rollback or failed-handover recovery.
- `app.lock` serializes application lifetimes; `update.lock` protects state and executable changes. All processes for an installation must share the same state directory.

Unreadable or invalid state is preserved rather than silently replaced. Do not edit or delete state while the application or its service manager can start another instance.

## Refresh an expiring manifest

A quiet channel still needs expiry renewal:

```sh
go run ./cmd/release refresh --channel stable --ttl 720h
```

The command verifies the existing signature and artifact hashes before re-signing. It refuses modified content. A practical deployment can schedule weekly refresh and alert when `/status` reports expiry within seven days; scheduling and alerting infrastructure are not included here.

Expired manifests do not stop the workload. Clients reject the update, log the error, and retry with backoff.

## Update-server outage

1. Inspect `GET /healthz`, configuration, TLS termination, and request logs.
2. Restore the server and its release files from trusted storage. Preserve endpoint routing and manifest caching policy.
3. Allow clients to retry. Failed checks back off exponentially up to the larger of one hour and the configured interval, with ±20% jitter. Recovery time includes that remaining backoff and normal download/startup time.

Clients keep their current workload running while the update server is unavailable.

## Signing-key rotation and compromise

### Planned rotation

1. Generate a new key:

   ```sh
   go run ./cmd/release keygen --key-id k2 --out keys
   ```

2. Create a public-key-ring file containing both entries on one comma-separated line:

   ```sh
   printf '%s,%s\n' "$(cat keys/k1.pub)" "$(cat keys/k2.pub)" > keys/rotation.pub
   make build VERSION=2.0.0 PUBKEYS=keys/rotation.pub
   go run ./cmd/release publish --version 2.0.0 --dist dist/2.0.0 --key keys/k1.key
   ```

3. Once clients have received the dual-key build, sign subsequent releases with `k2`.
4. Later publish a build embedding only `keys/k2.pub`, signed with `k2`. Keep an upgrade path for clients that remained offline on old-key-only builds.

`TestKeyRotation` verifies acceptance with a dual-key ring and rejection of a retired key after narrowing trust. It does not establish fleet adoption.

### Compromise response

An attacker with a trusted signing key and a way to serve releases can authorize malicious code. Secure the publishing infrastructure, preserve evidence, generate a new key, and assess which clients may already have installed attacker-controlled code.

For clients that still run trusted code and accept only `k1`, a possible migration is a higher-version build embedding only `k2`, authenticated with `k1`:

```sh
make build VERSION=2.0.1 PUBKEYS=keys/k2.pub
go run ./cmd/release publish --version 2.0.1 --dist dist/2.0.1 \
  --key keys/k1.key --min-version 2.0.1
```

This is not an immediate revocation mechanism. An attacker can race the migration, publish a higher version, or retain access to offline clients. Compromised clients require separate incident recovery. An offline root-key hierarchy is future work.

## Manual recovery of one installation

1. Stop the application and disable any external restart policy temporarily. Halt the bad rollout or publish a corrected higher version first, so the repaired client is not immediately offered the same bad release.
2. Preserve the executable, `.prev`, and state where readable, along with relevant logs, in a private incident directory. Keep the originals available for diagnosis.
3. Restore only a trusted executable. On macOS/Linux, with all instances stopped and from the installation directory:

   ```sh
   cp -p app.prev app
   ```

   On Windows use the corresponding `app.exe.prev` and `app.exe` paths. If `.prev` is missing or invalid, install a verified build through the trusted distribution process instead.
4. Keep valid updater state. If state is damaged, preserve its bytes before an explicit reset. Deleting it loses the high-water mark, blocklist, and pending recovery information; this is an operator decision, not routine cleanup.
5. Restart the repaired app, inspect its version and logs, and verify a healthy update path before re-enabling service restarts.

A Windows interruption between executable renames can leave the install path missing. Recover from the trusted `.prev` backup or another verified build; startup cleanup cannot repair an executable that cannot be launched.
