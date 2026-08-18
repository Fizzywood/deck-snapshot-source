# Deck Snapshot User Guide

Deck Snapshot is a local-first SteamOS Desktop Mode application. Local backups do not require Google Drive. The v0.1.6 candidate adds focused snapshot management and a user-initiated verified updater while keeping the verified backup, cloud, OAuth, snapshot, and restore contracts unchanged.

## Install on SteamOS

1. Download `deck_snapshot_installer.desktop` from the official Deck Snapshot GitHub release.
2. In the file properties, enable **Is executable** if Plasma does not already show the file as executable.
3. Open the installer. It downloads the matching Linux archive and checksum from the same official release, validates SHA-256, and installs only for the current user.

Wait for the installer result before opening it again. A second launch while installation is active is rejected with a clear message and does not start another download or installation.
4. Open **Deck Snapshot** from the application launcher in Desktop Mode.

For a private prerelease or an offline local-network validation, place all three matching release assets in the XDG Downloads directory before opening the installer. The installer may then use the local archive and checksum, but only when both are regular files beneath that directory and the checksum matches the exact archive digest embedded in the versioned installer. It never trusts an arbitrary local checksum as the release identity.

When Deck Snapshot opens, it performs one bounded foreground check of the fixed verified public channel. If a newer stable version is available, **Update to vX.Y.Z** appears in the main menu; selecting it shows the installed and available versions before **Update now** starts the verified installer. There is no update daemon, scheduled polling, repeated menu-navigation check, or automatic installation. **Settings & tools → Application → About & Updates** remains the manual status/check page. The installer is downloaded privately, verified against the release manifest SHA-256, and only then started; backups and settings are kept. If a check is unavailable, Deck Snapshot continues to work normally. Installers and release assets come from the public release repository. Clean source snapshots for public releases are available separately from [the public source repository](https://github.com/TAndrson/deck-snapshot-source); the development repository and its history remain private. Deck Snapshot never asks users for a GitHub token. See [`update-channel-v0.1.1.md`](update-channel-v0.1.1.md).

The application is installed below `~/.local/lib/deck-snapshot`. Its launcher and named icon are installed in the user-local XDG application and icon directories. The installer does not request root access and does not modify the read-only SteamOS system partition.

## Main screen

The primary menu contains only **Create Backup**, **Snapshots**, **Settings & tools**, and **Quit**. **Settings & tools** contains **Backup settings**, **Google Drive**, **Application**, and **Diagnostics**. Google Drive recovery export/import and disconnect are grouped under **Google Drive → Advanced**. The legacy v0.1.0 recovery action appears only when a supported legacy configuration is actually detected.

The dashboard reports Google Drive connection separately from the latest local backup's storage. **Stored: Local + Cloud** means the latest local backup was also confirmed in Google Drive. **Stored: Local only** means the validated local copy is available even if Drive is disconnected, automatic upload is off, or the latest upload did not complete.

## Create a local snapshot

1. Open Deck Snapshot.
2. Choose **Create Backup**.
3. Keep the progress window open while Deck Snapshot creates and validates the local backup. If automatic Drive upload is enabled, the same flow then reports the separate protected-upload phase. The archive is published only after its manifest, file inventory, resource limits, and checksums pass.
4. Choose **Snapshots** to validate and inspect it. **Validation** confirms archive integrity. If **Notices** are shown, choose **View details** to review grouped capture or compatibility limits before relying on a complete restore. From the summary, choose **Restore this backup** to continue directly to the existing exact restore-plan flow.

Local snapshots are stored below `~/.local/share/deck-snapshot/snapshots` by default.

Only one backup can run at a time. A duplicate request is rejected before snapshot work starts. If Google Drive upload fails, the validated local backup remains safe; Deck Snapshot offers an immediate upload retry and accurately reports **Local only** if the cloud copy is not completed.

## Prepare protected Google Drive access

Deck Snapshot stores protected snapshots at the fixed visible path `My Drive/Deck Snapshot/Snapshots/`. It uses rclone crypt for client-side content and name protection. Google authorization requests exactly `drive.file` for the visible snapshot files and `drive.appdata` for one private application recovery object. The release contains the app's Google Desktop installed-app credential. Users do not create or enter developer credentials or a cloud-configuration password.

To connect a Google account:

1. Choose **Settings & tools → Google Drive → Connect or reconnect**.
2. Complete authorization in the system browser. Deck Snapshot uses Authorization Code with PKCE S256, a private loopback callback, and only the two scopes above.
3. For a new account with no Deck Snapshot cloud backups, confirm the one-time setup prompt. Deck Snapshot creates encrypted recovery material, stores it in Google's private application area, reads it back, and verifies the fingerprint before enabling cloud backups.

An existing installation must reconnect once so Google can grant the new private recovery scope. Existing encrypted snapshots are left unchanged during this consent and migration step.

After a SteamOS reinstall, install Decky Loader and Deck Snapshot, connect the same Google account, and the managed recovery object is retrieved automatically. Existing encrypted snapshots can then be listed and restored through the normal validation and restore-plan flow.

Snapshots are encrypted client-side. The recovery key is stored in the application's private Google Drive configuration area so the same Google account can recover backups after reinstalling SteamOS. This protects a copied ciphertext snapshot and ordinary visible Drive access, but it does not protect against full compromise of the same Google account, which can expose both the ciphertext and recovery material.

If the private recovery object is missing, corrupt, or belongs to a conflicting duplicate, Deck Snapshot stops safely and offers the Advanced **Import recovery key** fallback. An existing v0.1.3/v0.1.4 recovery JSON remains valid; it is verified against an existing encrypted snapshot and uploaded to the private application area without re-encrypting or replacing any snapshot. **Export recovery key** remains available as an optional offline fallback; the original file is never overwritten or deleted.

Deck Snapshot generates a private local key for its encrypted OAuth configuration. No user password or credential entry is involved. The embedded Desktop credential is never added to the authorization URL or a runtime command line. The authorization code, PKCE verifier, and token are never placed on command lines or written to plaintext temporary files. The installed signed browser adapter accepts only the bounded Google authorization shape and scrubs cloud-secret environment fields before launching the registered system browser. The resulting token and refresh configuration are written only into the already encrypted private rclone configuration through a short-lived local Unix socket. The appData recovery object is not contacted on every backup; it is used during setup and reconnect/recovery. Cloud upload remains disabled until the connected crypt wrapper and matching recovery acknowledgement pass.

Connection success creates and reads back the protected fixed Drive folder and verifies the private recovery object. The dashboard and diagnostics perform a live bounded Drive read, so revoked authorization, offline access, or provider errors are shown as requiring attention rather than inferred as connected from a local config file.

### Existing v0.1.0 cloud backups

v0.1.0 used Google's hidden app-folder scope. When a supported legacy configuration is detected, choose **Settings & tools → Google Drive → Advanced → Legacy v0.1.0 recovery** and enter its existing configuration password once. Deck Snapshot validates that complete legacy connection before retaining the password as private local auth state.

Before disconnecting a legacy connection, Deck Snapshot preserves its encrypted OAuth configuration and local unlock key in a private migration slot. The old Drive data is not removed. The **Snapshots** browser can explicitly list and download those legacy backups while a new visible-folder connection is active. New uploads through the legacy connection are always rejected.

If the old provider client is already invalid or revoked, **Connect / Reconnect Google Drive** asks for the existing configuration password once and validates the encrypted local legacy profile before preserving it. Provider failure is never presented as a successful cloud check, and the active password is not retained; the preserved migration slot remains available if legacy authorization can later be recovered.

## Upload and download

- Enable automatic upload in **Settings & tools → Backup settings**. For a selected local-only backup, **Snapshots** also offers **Upload to Google Drive**. Every upload is downloaded again, validated, and byte-compared before success is reported. An existing cloud object is never replaced.
- Choose **Snapshots** for one catalog of local, current-cloud, and retained legacy-cloud snapshots. Rows use your Deck's local time (for example, **Today · 14:40**) while archive names remain unchanged. A cloud-only selection is inspected through a private temporary validated download and does not become a persistent local backup unless you choose restore.

For a selected backup, **Delete backup…** is always explicit. Local deletion permanently removes only that one validated archive from Deck Snapshot's snapshot directory. A normal Google Drive deletion moves only the selected cloud copy to Google Drive Trash and verifies it has left the active listing; it is never a permanent, wildcard, directory, or legacy-cloud deletion.

## Review and run a restore

1. Close Steam and return to Desktop Mode before a real restore.
2. Choose **Snapshots**, inspect a validated backup, then choose **Restore this backup** from its summary to create an exact restore plan.
3. Review the plain-language change summary. Use **Technical details** only when you need the exact plan information.
4. Do not continue if Deck Snapshot says it cannot continue safely.
5. Confirm the restore only when ready. Deck Snapshot revalidates the sealed plan and target, creates and fully validates a safety backup, restores the supported customization scope, refreshes the supported runtime, and verifies the result.
6. Keep the reported recovery snapshot and restore report.
7. Return to Gaming Mode only after the report is successful or its warnings are understood.

The first restore to real Steam/Decky paths on a Steam Deck is a deliberate validation checkpoint. Test the sandbox-target flow first and approve a real restore explicitly.

If Decky Loader is not installed, Deck Snapshot explains that it is required for restoring Decky plugins and settings. **Get Decky** opens the official Decky Loader page, and **Check again** refreshes detection. Deck Snapshot never installs or modifies Decky.

The exact v0.1.0 restore engine passed two separately approved production runs, recovery validation, reboot, idempotence, and physical visible-state confirmation on the real Steam Deck. v0.1.1 does not weaken or bypass those gates. Every new real production restore still requires a newly reviewed exact plan and explicit approval.

## Diagnostics

Choose **Settings & tools → Diagnostics** for a short readiness summary covering Steam, Decky, Google Drive, cloud storage, local snapshot storage, and disk space. A redacted technical report remains available on request.

## Disconnect and uninstall

**Settings & tools → Google Drive → Connect or reconnect** can explicitly forget an unusable local authorization and open the normal Google browser flow again. This works even when the provider token is revoked or the provider is temporarily unreachable. It does not revoke the provider grant or delete any cloud or local snapshot.

The advanced **Disconnect Google Drive locally** action forgets only the active local connection and acknowledgement. It does not revoke the provider grant, delete the appData recovery object, or delete any cloud snapshot. A legacy v0.1.0 connection is privately retained for migration before it is disconnected.

Choose **Settings & tools → Application → Uninstall Deck Snapshot** and confirm. The same bounded uninstaller is also available at `~/.local/lib/deck-snapshot/uninstall.sh`. It removes only Deck Snapshot's application binaries, launcher, and named icon. Local backups, recovery snapshots, reports, cloud configuration, managed recovery material, exported fallback recovery files, and unrelated user icons are preserved intentionally.

## Development disclosure

Deck Snapshot was designed and implemented with substantial AI assistance under human product ownership. Releases remain subject to automated tests, focused security review, checksum verification, and real Steam Deck validation. AI assistance and testing do not guarantee that the software is error-free; keep independent copies of important data and review every restore plan.
