# Snapshot Format v1

Status: implemented in Phase 2 and consumed by the Phase 3 transactional restore engine.

## Container

A Deck Snapshot v1 file is a gzip-compressed POSIX tar archive named:

```text
deck-snapshot-<UTC timestamp>-<snapshot ID>.tar.gz
```

`manifest.json` is the first entry. Every later entry is a regular file declared exactly once in `manifest.files`. Directory entries, symlinks, hardlinks, devices, FIFOs and other special entries are rejected. The current writer emits no archive-provided directories or links.

The writer uses Go's standard-library tar and gzip implementations. It writes a private temporary file in the final directory, closes and syncs it, validates the completed archive from disk, and then publishes it with a held-directory, same-filesystem atomic no-replace rename. On Linux, the destination directory is synchronized after publication. A collision, failure, cancellation, or invalid creation never overwrites or exposes a partial final snapshot.

## Logical layout

Current payload paths are:

```text
manifest.json
decky/settings/loader.json
decky/settings/<plugin directory>/...
decky/data/<plugin directory>/...
css-loader/themes/...
steam/artwork/userdata/<account ID>/grid/...
steam/artwork/librarycache/<selected icon>
reports/discovery.json
reports/warnings.json
```

Installed plugin binaries are metadata inputs, not payload. The complete Steam library cache is never captured. `shortcuts.vdf` is excluded until Phase 3 can prove a preserving binary-VDF merge; it is never overwritten wholesale.

## Manifest

`format_version` is currently `1.0`. Readers reject an unknown major version. The manifest records:

- immutable snapshot ID and Deck Snapshot app version;
- UTC and local creation timestamps;
- stable random non-sensitive device ID and optional display name;
- host OS and architecture plus detected product versions when known;
- synthetic-safe Steam account identifiers, Decky plugin metadata, CSS theme/profile metadata and artwork metadata;
- every included payload path, component, byte size, original permission bits and SHA-256;
- explicit exclusions, English warnings and compatibility gates.

Source filesystem paths and file contents are not copied into reports. OAuth/rclone state and recovery material are never part of this format.

## Path rules

Logical paths always use `/`. A reader rejects:

- empty, `.` or non-normalized paths;
- absolute, UNC or drive-qualified paths;
- backslashes, NULs and control characters;
- `.`/`..` traversal;
- duplicate paths and undeclared entries;
- paths over 1,024 bytes.

These checks happen before a future restore is allowed to join a logical path to any target root. Phase 3 must independently enforce allowlisted target containment and existing-component symlink checks.

## Default resource limits

| Limit | v1 default |
| --- | ---: |
| Payload files | 20,000 |
| One payload file | 128 MiB |
| Total payload | 2 GiB |
| Manifest | 8 MiB |
| Logical path | 1,024 bytes |
| Uncompressed/compressed ratio | 200:1 |

Limits apply during discovery, creation and validation. The validator also forces full gzip checksum verification, rejects trailing data/additional gzip members, requires exact entry sizes, and hashes every payload while streaming it.

## Discovery classification

Discovery never follows a symbolic link. Non-regular files are excluded with a warning. A supported text configuration file over 1 MiB is excluded because it cannot pass the bounded credential scan. Known secret-like filenames and supported text files containing likely token/password/key fields are excluded entirely and reported without their contents.

The CSS Loader adapter captures the verified themes tree, including profiles, configuration, priority, `STORE` state and resources. The Steam adapter captures image files in numeric userdata grid directories, selected numeric library-cache icon files, and two narrowly validated grid sidecars: version-1 logo-position JSON metadata (maximum 4 KiB, fixed schema) and numeric `_icon.ico` files with a PNG or ICO header (maximum 1 MiB). It does not capture `shortcuts.vdf` or arbitrary Steam state. Exact Steam client filename semantics remain marked unverified until real current hardware validation.

## Validation versus restore

Successful v1 validation proves container structure, declared inventory, configured resource bounds and payload integrity. It does not prove compatibility with a target Deck, plugin package safety, or permission to mutate production paths. Restore planning, recovery snapshots, target allowlists, plugin resolution and actual writes remain Phase 3 gates.
