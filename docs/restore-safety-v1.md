# Restore Safety Contract v1

Phase 3 implements fixture-proven local restore on Linux. It has not been validated on real Steam Deck hardware, and it does not stop or restart Steam or Decky automatically. Snapshot creation, inspection, validation, and restore planning remain available on Windows, but `restore run` fails closed there because the required atomic exchange and directory-durability contract is currently implemented only on Linux.

## Review and approval

`restore plan` fully validates the selected snapshot, maps every supported payload to an allowlisted target, fingerprints the snapshot and current target state, resolves each plugin against the official Decky store, calculates conservative temporary-space requirements, and writes an immutable private plan. Planning does not create or modify Steam, Decky, CSS Loader, or artwork targets.

The plan displays two approval values:

- a short plan ID derived from the complete canonical plan;
- the complete SHA-256 approval hash.

`restore run` requires both exact values and the exact plan file. Immediately before staging, before recovery, and before the first target mutation, it validates the plan, snapshot identity, checksums, target fingerprints, target containment, and existing path components again. Any collision or stale state blocks the run.

## Target mappings

Only these snapshot prefixes are eligible for production restore:

```text
decky/settings/*                    -> <Decky>/settings/*
decky/data/*                        -> <Decky>/data/*
css-loader/themes/*                 -> <Decky>/themes/*
steam/artwork/librarycache/*        -> <Steam>/appcache/librarycache/*
steam/artwork/userdata/<id>/grid/*  -> <Steam>/userdata/<id>/config/grid/*
```

Generated reports are not restored. Any other payload prefix fails planning. `shortcuts.vdf` has no mapping and remains unmodified.

Every target must remain beneath its narrow mapped root. Decky, Steam, and application-state roots must be distinct, non-overlapping directories beneath the selected target home. Existing path components may not be symlinks, junctions/reparse points, files, unowned directories, or other special entries. Restore writes use Go's confined `os.Root` operations in addition to explicit containment, ownership, and writable-ancestor checks.

## Recovery and transaction behavior

Selected snapshot payloads are copied to opaque private staging filenames; archive logical paths are never passed to a generic extraction helper. The complete snapshot inventory, types, sizes, checksums, compression ratio, stream trailer, and file identity are verified while staging.

Before the first customization-target mutation, Deck Snapshot creates and validates an immutable recovery snapshot containing:

- every existing regular file that a file action will replace;
- every regular file in an existing plugin directory that will be replaced;
- explicit exclusions recording targets that were absent before restore.

The recovery snapshot is retained after success or failure, and its exact path is included in the restore report. Its private temporary file and the destination directory entry are synchronized before the first production mutation.

Regular files are written through cryptographically named, mode-`0700` private transaction directories on the target filesystem. Directory handles stay open for the complete mutation. New files are published with a kernel no-replace operation. Existing files are replaced with Linux `RENAME_EXCHANGE`, so the target pathname always names either the old or new file; the displaced inode is verified before cleanup. Source and destination directories are synchronized at every committed namespace transition. A failed file action rolls back every earlier file action in reverse order through the same exchange or no-replace primitives. Created files are removed only after the exact applied payload has been moved into and verified inside the held private directory; replaced files are restored from the already validated recovery snapshot through the same confined path.

For a current-user-owned writable plugin tree, existing plugin directories are never recursively overwritten or deleted by production pathname. Verified packages are staged on the Decky plugin filesystem. Replacements atomically exchange the prepared and active directories, synchronize both parents, verify the displaced original, and then durably move that exact directory to its persistent application-state preservation path. Rollback atomically exchanges the active replacement and preserved original before quarantining the verified generated directory through held handles. A crash cannot expose an absent active-plugin pathname: it leaves either the old or new directory active. Plugin creation uses an atomic no-replace move.

Decky Loader v3.2.6 normally owns the real plugin root. Planning therefore first fingerprints every existing plugin entry without writing it, permits only root or the current user as owner, and rejects links, special files, shared-write permission bits, identity mismatches, and resource-limit violations. A verified current-stable name, author, version, and tree is classified as unchanged without requiring target-root write access or opening a WebSocket.

A genuinely missing or outdated plugin on a non-writable safe root uses only the exact Decky Loader v3.2.6 official loopback flow. The adapter is fixed to `127.0.0.1:1337`, disables proxies and redirects, validates the private local ZIP and exact SHA-256 again, keeps the opened Linux archive inode addressable through `/proc/<pid>/fd/<fd>`, obtains a bounded UUID token without logging it, matches the exact confirmation prompt, and requires the matching completion event. The resulting target metadata and complete safe content fingerprint must match the approved package. Other loader versions fail closed. Decky's official installer is not claimed to provide the same kernel atomic-exchange guarantee as the direct writable-tree method; the already published validated recovery snapshot is retained throughout, and any uncertain result is reported as rollback failure.

Decky's update flow can alter `decky/settings/loader.json` while removing an earlier plugin. Any plan containing a mutable Decky API plugin therefore binds a separate recovery guard to the current target: an existing regular, user-owned file is fingerprinted and copied into the validated recovery snapshot, while an absent file is recorded as an exclusion. That exact pre-operation state is reinstated immediately after every API operation, including rollback. This guard is independent of the source snapshot, so legacy snapshots remain usable without trusting unvalidated settings bytes. A validated private ZIP reconstructed from recovery payloads is used to reinstall an earlier plugin during rollback, followed by exact original-tree and metadata verification.

Private cleanup is performed only through held directory handles and only for content proven to be tool-generated or already represented in the validated recovery snapshot. If any moved identity or restoration is uncertain, cleanup stops, the private transaction is retained, the report includes its exact path, and the run is marked `rollback_failed`. Crash-orphaned transaction directories are not automatically purged.

## Plugin trust chain

The default resolver reads the [current official Decky store catalog](https://plugins.deckbrew.xyz/plugins) from:

```text
https://plugins.deckbrew.xyz/plugins
```

The implementation mirrors the current Decky behavior that the first stable version is current, but requires a unique visible name-and-author identity. Missing, ambiguous, author-mismatched, or invalid entries block the plan. The snapshot version remains visible; no version is silently substituted.

Package artifacts require a credential-free, query-free HTTPS URL and a valid SHA-256 value from store metadata. Redirects are checked before following. Every request has a mandatory two-minute upper bound, honors cancellation, is size-bounded, and is written privately. ZIP validation rejects traversal, absolute/drive paths, non-NFC paths, Windows alternate-data-stream and reserved-device names, trailing dot/space segments, case collisions, duplicate entries, links, special files, multiple top-level directories, wrong package identity, size/count/ratio violations, undeclared `bin` content, unsafe remote-binary declarations, and checksum mismatches. Package content is never executed by Deck Snapshot.

The current implementation was checked against the official Decky store and [Decky Loader source revision `b4b8be3297e427dad6fbc6697ffdb765a796f7fd`](https://github.com/SteamDeckHomebrew/decky-loader/tree/b4b8be3297e427dad6fbc6697ffdb765a796f7fd). Current store values are runtime data and are never hardcoded as trusted releases.

## Settings compatibility

When the verified current stable plugin version differs from the snapshot version, generic plugin settings/data are not blindly applied. They are preserved under:

```text
<state>/incompatible/<snapshot-id>/<snapshot-logical-path>
```

The plan and report explain the old and new versions and the preservation path. An existing different file at that path is a blocking collision. A future plugin-specific compatibility adapter may promote preserved data only with explicit tests.

## Resource limits

Snapshot limits remain the limits documented in `snapshot-format-v1.md`. Plugin packages additionally enforce:

- 256 MiB maximum compressed download;
- 10,000 ZIP entries;
- 128 MiB per file;
- 512 MiB aggregate extracted data;
- 200:1 maximum entry and aggregate compression ratio.

The free-space plan includes every simultaneous recovery archive/staged copy, selected-payload staging, same-filesystem write transactions, prepared plugin downloads/extraction, existing-plugin preservation, and a fixed 64 MiB safety reserve with checked arithmetic. The full conservative requirement is checked on every involved Home, State, Decky, and Steam filesystem before staging or target mutation. If free space cannot be determined or is insufficient, restore stops.

## CLI

```text
deck-snapshot restore plan [--home PATH] [--decky-home PATH] [--steam-home PATH] [--json] SNAPSHOT

deck-snapshot restore run \
  --approve RESTORE_PLAN_ID \
  --approval-hash FULL_SHA256 \
  [--json] \
  RESTORE_PLAN_FILE
```

Use only synthetic or sandbox target homes until the Phase 5 hardware sequence reaches its explicit live-restore approval checkpoint.

## Independent security review

The Phase 3 focused review completed on 2026-08-14. All reported Critical and High findings were fixed and re-reviewed; no unresolved Critical or High finding remains. The review and acceptance evidence are maintained in GitHub Issue `#6` together with the CI results. This is fixture and CI evidence only, not a claim of real Steam Deck validation.
