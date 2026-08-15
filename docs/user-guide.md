# Deck Snapshot User Guide

Deck Snapshot is a local-first SteamOS Desktop Mode application. Local backups do not require Google Drive. v0.1.2 is the current verified release; v0.1.3 adds focused backup progress and public-source transparency while keeping the verified backup, cloud, OAuth, and restore contracts unchanged.

## Install on SteamOS

1. Download `deck_snapshot_installer.desktop` from the official Deck Snapshot GitHub release.
2. In the file properties, enable **Is executable** if Plasma does not already show the file as executable.
3. Open the installer. It downloads the matching Linux archive and checksum from the same official release, validates SHA-256, and installs only for the current user.

Wait for the installer result before opening it again. A second launch while installation is active is rejected with a clear message and does not start another download or installation.
4. Open **Deck Snapshot** from the application launcher in Desktop Mode.

For a private prerelease or an offline local-network validation, place all three matching release assets in the XDG Downloads directory before opening the installer. The installer may then use the local archive and checksum, but only when both are regular files beneath that directory and the checksum matches the exact archive digest embedded in the versioned installer. It never trusts an arbitrary local checksum as the release identity.

The dashboard reports **No automatic updates**. Deck Snapshot never downloads or installs an update in the background. To update, download the newest installer from the [verified public release page](https://github.com/Fizzywood/deck-snapshot-releases/releases/latest) and open it; existing backups and settings are kept. Installers and release assets come from the public release repository. Clean source snapshots for public releases are available separately from [the public source repository](https://github.com/Fizzywood/deck-snapshot-source); the development repository and its history remain private. Deck Snapshot never asks users for a GitHub token. Background checks and automatic installation remain fail-closed as documented in [`update-channel-v0.1.1.md`](update-channel-v0.1.1.md).

The application is installed below `~/.local/lib/deck-snapshot`. Its launcher and named icon are installed in the user-local XDG application and icon directories. The installer does not request root access and does not modify the read-only SteamOS system partition.

## Main screen

The primary menu contains only **Create Backup**, **Restore**, **Snapshots**, **Settings**, **Diagnostics**, and **More options**. Google Drive connection and recovery actions are grouped under **More options → Google Drive**. Migration, manual transfer, disconnect, about, and uninstall actions remain available in the nested advanced or application menus without crowding the normal backup flow.

The dashboard reports Google Drive connection separately from the latest local backup's storage. **Stored: Local + Cloud** means the latest local backup was also confirmed in Google Drive. **Stored: Local only** means the validated local copy is available even if Drive is disconnected, automatic upload is off, or the latest upload did not complete.

## Create a local snapshot

1. Open Deck Snapshot.
2. Choose **Create Backup**.
3. Keep the progress window open while Deck Snapshot creates and validates the local backup. If automatic Drive upload is enabled, the same flow then reports the separate protected-upload phase. The archive is published only after its manifest, file inventory, resource limits, and checksums pass.
4. Choose **Snapshots** to validate and inspect it. Choose **Restore** separately when you intend to prepare an exact restore plan.

Local snapshots are stored below `~/.local/share/deck-snapshot/snapshots` by default.

Only one backup can run at a time. A duplicate request is rejected before snapshot work starts. If Google Drive upload fails, the validated local backup remains safe; Deck Snapshot offers an immediate upload retry and accurately reports **Local only** if the cloud copy is not completed.

## Prepare protected Google Drive access

Deck Snapshot stores protected snapshots at the fixed visible path `My Drive/Deck Snapshot/Snapshots/`. It uses rclone crypt for client-side content and name protection and requests exactly the Google `drive.file` scope, which limits access to files and folders created for the app. The release contains the app's Google Desktop installed-app credential. Users do not create or enter developer credentials or a cloud-configuration password.

Before connecting:

1. Choose **More options → Google Drive → Create recovery material** and save the JSON file somewhere separate from Google Drive snapshots. A private local folder or separate removable media is appropriate.
2. Back up that recovery file separately. Losing it makes cloud snapshots unrecoverable. Anyone who has both the file and access to the app-folder data can decrypt snapshots.
3. Choose **More options → Google Drive → Connect or reconnect**.
4. Select the recovery file. Deck Snapshot remembers only its local path; the recovery-key contents stay in that separate private file.
5. Complete authorization in the system browser. Deck Snapshot uses Authorization Code with PKCE S256, a private loopback callback, and only `drive.file`.

Deck Snapshot generates a private local key for its encrypted OAuth configuration. No user password or credential entry is involved. The embedded Desktop credential is never added to the authorization URL or a runtime command line. The authorization code, PKCE verifier, and token are never placed on command lines or written to plaintext temporary files. The installed signed browser adapter accepts only the bounded Google authorization shape and scrubs cloud-secret environment fields before launching the registered system browser. The resulting token and refresh configuration are written only into the already encrypted private rclone configuration through a short-lived local Unix socket. Cloud upload remains disabled until the connected crypt wrapper and matching recovery acknowledgement pass.

Connection success creates and reads back the protected fixed Drive folder. The dashboard and diagnostics perform a live bounded Drive read, so revoked authorization, offline access, or provider errors are shown as requiring attention rather than inferred as connected from a local config file.

### Existing v0.1.0 cloud backups

v0.1.0 used Google's hidden app-folder scope. Choose **More options → Google Drive → Advanced actions → Unlock a v0.1.0 cloud connection** once and enter its existing configuration password only when upgrading an already configured installation. Deck Snapshot validates that complete legacy connection before retaining the password as private local auth state.

Before disconnecting a legacy connection, Deck Snapshot preserves its encrypted OAuth configuration and local unlock key in a private migration slot. The old Drive data is not removed. The **Snapshots** browser can explicitly list and download those legacy backups while a new visible-folder connection is active. New uploads through the legacy connection are always rejected.

If the old provider client is already invalid or revoked, **Connect / Reconnect Google Drive** asks for the existing configuration password once and validates the encrypted local legacy profile before preserving it. Provider failure is never presented as a successful cloud check, and the active password is not retained; the preserved migration slot remains available if legacy authorization can later be recovered.

## Upload and download

- Enable automatic upload in **Settings**, or choose the manual upload action below **More options → Google Drive → Advanced actions**. Every upload is downloaded again, validated, and byte-compared before success is reported. An existing cloud object is never replaced.
- Choose **Snapshots** for one catalog of local, current-cloud, and retained legacy-cloud snapshots. A cloud-only selection is downloaded and fully validated before details or restore planning are offered. An existing local snapshot is never replaced.

Deck Snapshot does not delete cloud snapshots in v0.1.

## Review and run a restore

1. Close Steam and return to Desktop Mode before a real restore.
2. Choose **Snapshots** if you first want to inspect a validated backup. Choose **Restore** when you are ready to create and review an exact restore plan.
3. Read every file action, plugin action, warning, required-space value, plan ID, and approval hash.
4. Do not continue if the plan is blocked or targets are unexpected.
5. Confirm the exact plan only when ready. Deck Snapshot revalidates the plan and target, creates and fully validates a recovery snapshot, and only then starts the bounded transaction.
6. Keep the reported recovery snapshot and restore report.
7. Return to Gaming Mode only after the report is successful or its warnings are understood.

The first restore to real Steam/Decky paths on a Steam Deck is a deliberate validation checkpoint. Test the sandbox-target flow first and approve a real restore explicitly.

The exact v0.1.0 restore engine passed two separately approved production runs, recovery validation, reboot, idempotence, and physical visible-state confirmation on the real Steam Deck. v0.1.1 does not weaken or bypass those gates. Every new real production restore still requires a newly reviewed exact plan and explicit approval.

## Diagnostics

Choose **Diagnostics** for a short readiness summary covering Steam, Decky, Google Drive, cloud storage, local snapshot storage, and disk space. A redacted technical report remains available on request.

## Disconnect and uninstall

**More options → Google Drive → Connect or reconnect** can explicitly forget an unusable local authorization and open the normal Google browser flow again. This works even when the provider token is revoked or the provider is temporarily unreachable. It does not revoke the provider grant or delete any cloud or local snapshot.

The advanced **Disconnect Google Drive locally** action forgets only the active local connection and acknowledgement. It does not revoke the provider grant or delete any cloud snapshot. A legacy v0.1.0 connection is privately retained for migration before it is disconnected.

Choose **More options → Application → Uninstall Deck Snapshot** and confirm. The same bounded uninstaller is also available at `~/.local/lib/deck-snapshot/uninstall.sh`. It removes only Deck Snapshot's application binaries, launcher, and named icon. Local backups, recovery snapshots, reports, cloud configuration, separate recovery material, and unrelated user icons are preserved intentionally.

## Development disclosure

Deck Snapshot was designed and implemented with substantial AI assistance under human product ownership. Releases remain subject to automated tests, focused security review, checksum verification, and real Steam Deck validation. AI assistance and testing do not guarantee that the software is error-free; keep independent copies of important data and review every restore plan.
