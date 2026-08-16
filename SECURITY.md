# Security Contract

Deck Snapshot handles user customization data and writes into Steam/Decky-controlled paths. Restore is the highest-risk operation. These rules are binding for implementation, review and release.

## Trust model

Treat snapshot archives, manifests, plugin metadata, cloud listings, downloaded binaries/packages, existing filesystem contents and external-process output as untrusted. Treat plugin settings as potentially sensitive. A valid checksum proves integrity against expected metadata, not that content is safe to extract or execute.

## Snapshot creation

- Capture only explicitly classified files under discovered, supported roots.
- Record canonical source identity, size, mode where relevant and a cryptographic checksum for each regular file.
- Do not follow symlinks during discovery or capture. Record a safe diagnostic entry or exclude them explicitly.
- Reject device files, sockets, FIFOs and other special files.
- Apply per-file, total-size and file-count limits; make limits visible in reports.
- Write to a private temporary location, fsync where required, validate the completed snapshot, then publish it with an atomic rename on the same filesystem.
- On interruption, do not expose a partial archive as a valid snapshot.
- Exclude rclone configs, OAuth tokens, credential stores, recovery keys, private keys and known secret files regardless of plugin ownership.

## Manifest and archive validation

- Reject unknown major format versions and malformed or duplicate logical paths.
- Normalize logical paths using platform-independent rules before any filesystem join.
- Reject empty ambiguous paths, absolute paths, drive/UNC paths, `.`/`..` traversal, NUL/control characters and normalization collisions.
- Enforce that every archive entry is declared in the manifest and every required manifest file has exactly one archive entry.
- Verify size and checksum before a file is eligible for restore.
- Do not extract device files, hardlinks or archive-provided symlinks in v0.1.
- Bound decompressed size, compression ratio, entry count, path length and processing time to resist resource exhaustion.

## Restore targets and writes

- Build a restore plan without modifying production data.
- Resolve every target beneath an explicit allowlisted root and re-check containment after canonicalization.
- Validate every existing path component so a symlink/junction cannot redirect a write outside the root.
- Refuse ambiguous targets and report them; never guess.
- Create parent directories with restrictive, intentional permissions.
- Write regular files through private temporary files followed by an atomic replace where supported.
- Preserve incompatible settings separately; do not delete them or invent migrations.
- Never overwrite `shortcuts.vdf` wholesale. Any future merge must parse, preserve unknown fields, detect conflicts and be covered by fixture roundtrip and corruption tests.
- Do not execute content restored from a snapshot.

## Recovery and rollback

- Before any potentially destructive production restore, calculate required space and create a recovery snapshot limited to affected paths.
- Validate the recovery snapshot completely before the first production mutation.
- Keep recovery data after failures and include its exact user-visible location in the report.
- Stop dependent risky steps after a failure; isolate independent component failures only when continuation cannot worsen recovery.
- Rollback must use the same validation, planning and target-safety controls as restore.

## Deletion

- Prefer replacement and preservation over deletion.
- Any deletion must target an explicit, canonical path beneath a narrow allowlisted root and must be justified by the restore plan.
- Do not invoke recursive deletion tools with dynamic, unresolved, empty, home, root or workspace-wide targets.
- Never build destructive shell commands from snapshot, plugin or cloud metadata.

## External processes

- Prefer native APIs for filesystem operations.
- When a process is required, use an argument vector rather than a shell command string; never interpolate untrusted text into a shell.
- Set a minimal environment, working directory, timeout, output limit and cancellation behavior.
- Capture structured exit status while redacting secrets from command arguments, environment and output.
- Do not run plugin-provided executables or scripts from snapshots.

## Downloads and plugin packages

- Use HTTPS official sources resolved from verified metadata.
- Pin or verify publisher/source identity; validate cryptographic checksums or signatures when officially available.
- Download to a private temporary file, enforce size/time limits, verify before rename or execution, and fail closed on mismatch.
- Never silently substitute a package/version. Report the requested identity/version and the explicit alternative.
- The Decky installer adapter may use only the current official installation flow. Elevation must be isolated to the required official step and never granted to Deck Snapshot generally.

## OAuth, rclone and cloud encryption

- Use browser-based OAuth with the least practical Google Drive scope verified during discovery.
- Never commit the Google OAuth JSON or either Desktop credential value to source, and never print, log, report, snapshot, or publish them as standalone files or metadata.
- Treat Google's generated Desktop installed-app credential pair as non-confidential by provider design. Inject both values into verified builds only through masked CI/release secrets. Do not accept runtime developer credentials from normal users or create a plaintext credential sidecar.
- New Google authorization must use OAuth 2.0 Authorization Code with PKCE S256, a cryptographically random verifier and state, the exact Google authorization and token endpoints, and a dynamically allocated `127.0.0.1` loopback redirect. Reject a missing or mismatched state, a non-loopback callback, an unexpected response shape, redirecting token responses, and any granted scope other than the exact unordered set `https://www.googleapis.com/auth/drive.file` plus `https://www.googleapis.com/auth/drive.appdata`.
- Require exactly `drive.file` plus `drive.appdata` for new Google connections. Keep the rclone crypt wrapper bound to the fixed visible base path `My Drive/Deck Snapshot/Snapshots/`; use a native bounded Drive API client for the private recovery object. Never broaden the scope silently.
- A successful connection must create the fixed protected snapshot folder and read it back before recovery acknowledgement. Dashboard and diagnostic connection status must perform a bounded live read, not infer connectivity from local configuration alone.
- Store OAuth/rclone state outside snapshot roots with the most restrictive permissions supported by the platform.
- Never log tokens, authorization codes, refresh tokens, PKCE verifiers, OAuth state, authorization or callback URLs, passphrases, recovery keys, provider error descriptions, or unredacted rclone config. Redact transient OAuth URLs before external-process output reaches errors or reports.
- Protect cloud snapshots client-side by default. If protection cannot be established or verified, cloud upload must remain disabled.
- Never upload the recovery key/passphrase beside the encrypted backup in the same cloud location.
- A cloud download remains untrusted until decryption, manifest validation and checksum verification all succeed.
- Disconnect must remove only Deck Snapshot-owned local credentials after exact path validation; it must not revoke the provider grant or delete cloud snapshots. Provider revocation, if later offered, must be a separate explicit action with a rediscovery warning.
- Revoked or offline provider access must not prevent a separately confirmed local disconnect/reconnect. The local-only inspection path may be used solely to forget verified Deck Snapshot-owned configuration; it must not be presented as a successful live health check.
- Store crypt recovery secrets in the strictly validated, fixed-name `appDataFolder` recovery object and in an application-managed private local copy. Keep the separately exported recovery JSON as an optional Advanced fallback. At runtime, pass only rclone-obscured forms through the two exact allowlisted `RCLONE_CRYPT_PASSWORD*` environment fields; never persist production crypt secrets in `rclone.conf`, the visible snapshot folder, logs, diagnostics, or an argument vector. The same-Google-account recovery design does not protect against full compromise of that account.
- Encrypt the dedicated rclone configuration before OAuth writes a token. A fresh v0.1.1 connection generates a random private local wrapping key and supplies it only through initialization input and the exact `RCLONE_CONFIG_PASS` environment field. It is local OAuth state, never a user password or snapshot-recovery secret.
- Exchange the authorization code in-process using the embedded Desktop credential and never include its credential value in the authorization URL, a runtime argument vector, a normal process environment, output, logs, reports, or a plaintext temporary file. Transfer the resulting token and the credential needed for rclone refresh only in a bounded JSON request over a short-lived Unix socket inside a mode-`0700` private directory. Accept only the expected bounded non-interactive rclone configuration steps, encrypt and fsync the resulting config, and remove the private channel before continuing.
- A v0.1.0 configuration without the generated key may be unlocked once by accepting its existing password only through standard input, validating the complete protected remote first, then storing it as a private local key. Never accept it on an argument vector or persist an unverified value.
- If a legacy provider client is revoked or invalid, the same bounded standard-input password may be used only after local encryption/profile validation to preserve the encrypted legacy configuration and password in the private migration slot before local disconnect. It must not be installed as the active key or reported as live provider validation.
- Accept the v0.1.0 `drive.appfolder` layout only as a legacy read/download source for a verified migration. Disable new uploads through that layout and never remove its data automatically.
- Require a matching recovery-material fingerprint acknowledgement before first upload. The acknowledgement is valid only after the exact material was stored and read back from the fixed appData object, or explicitly imported and verified against existing ciphertext. An acknowledgement is not a key and must never substitute for recovery material.
- Cloud upload is immutable and complete only after a downloaded byte-for-byte roundtrip validates as the same snapshot. Cloud download validates in a private same-filesystem temporary file and publishes without replacement.

## Logs and diagnostics

- Use allowlisted structured fields instead of logging arbitrary configs, manifests or environment variables.
- Redact tokens, credentials, authorization headers, query secrets, home-account identifiers where unnecessary and sensitive setting values.
- Reports may include paths only when useful; provide a privacy-safe export mode and never embed file contents by default.
- Errors shown to users must remain actionable after redaction.

## Local permissions and SteamOS boundaries

- Prefer XDG user paths and private modes for configuration, state, temp data, snapshots and recovery data.
- Do not modify the read-only SteamOS system partition for ordinary installation or operation.
- Derive the current home directory; never assume `/home/deck`.
- Validate ownership and permissions after restore without broad recursive `chmod`/`chown` operations.
- Do not configure SSH, open network ports or expose the Steam Deck to the internet.

## Security testing gate

At minimum, automated tests must cover traversal, absolute/UNC paths, normalization collisions, symlink redirection, hardlinks/special files, checksum mismatch, corrupted/truncated archives, unknown format versions, size/count limits, interrupted writes, hostile external-process arguments, log redaction, recovery validation and idempotent restore. Phase 3 requires an independent focused review before acceptance.

## Incident and vulnerability handling

Do not publish real user data or exploit details in a public issue. Until a private reporting channel exists, stop release work, preserve minimal evidence without secrets and coordinate privately with the repository owner. Any critical unresolved restore, archive, credential or update-chain issue blocks release.
