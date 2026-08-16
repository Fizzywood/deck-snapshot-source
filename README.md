# Deck Snapshot Source

This repository contains clean, inspectable source snapshots for published Deck Snapshot releases.

**[Download Deck Snapshot](https://github.com/Fizzywood/deck-snapshot-releases/releases/latest)**

Deck Snapshot is a local-first SteamOS utility for backing up and restoring Steam Deck customization, including:

- Decky plugins and supported plugin state
- CSS Loader customization
- custom Steam artwork

It is not a full SteamOS image or a general-purpose backup utility.

## Why is the source public?

Deck Snapshot handles backups, Google Drive access, and restore operations.

The released source is published so users can inspect what the distributed application does and compare releases with the code used to build them.

This repository contains clean release snapshots only.

The private development repository, development history, issues, pull requests, internal coordination, hardware evidence, credentials, real snapshots, and user data are not published here.

## Releases

Each public source tag corresponds to the same Deck Snapshot release in the download repository.

**[Open the Deck Snapshot releases](https://github.com/Fizzywood/deck-snapshot-releases/releases)**

`SOURCE_VERSION` records the matching public release.

`SOURCE_PROVENANCE` records the release version and the exact private release commit used to generate the public source snapshot without exposing the private Git history.

## Build and test

Go 1.26 or later in the 1.26 release line is required.

```text
gofmt -w ./cmd ./internal ./tests
go vet ./...
go test ./...
go test -race ./...

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
go build -trimpath -o deck-snapshot-linux-amd64 ./cmd/deck-snapshot
```

Release builds additionally contain Google's Desktop installed-app OAuth credential through protected release configuration.

Desktop OAuth client credentials are non-confidential by design and may be extracted from a distributed desktop application. The standalone credential configuration is not published in this repository.

Deck Snapshot currently requests only:

- `drive.file`
- `drive.appdata`

These permissions are used for Deck Snapshot's own Drive files and private recovery data.

## Security

Cloud snapshots are encrypted client-side before upload.

Recovery information is stored in Deck Snapshot's private Google Drive app-data area so that encrypted backups can be recovered after reinstalling SteamOS by connecting the same Google account.

The same-account recovery model is not zero-knowledge protection against full compromise of that Google account.

Restore operations use snapshot validation, explicit planning, confirmation, path restrictions, and recovery safeguards.

For the detailed security contract, see [SECURITY.md](SECURITY.md).

## Licensing

Deck Snapshot is **source-available for transparency and inspection**.

It is **not an open-source project**.

All rights are reserved. No permission to reuse, modify, or redistribute Deck Snapshot is granted unless explicitly stated otherwise.

See [LICENSE](LICENSE).

Third-party components retain their own licenses. See [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

## AI-assisted development

Deck Snapshot was substantially designed and implemented with OpenAI Codex under human product ownership, an approach sometimes called vibe coding.

Releases are still subject to automated testing, focused security review, checksum verification, and validation on real Steam Deck hardware.

AI assistance and testing do not guarantee error-free software.

Deck Snapshot is an independent project and is not affiliated with or endorsed by Valve, Steam, Decky Loader, CSS Loader, Google, SteamGridDB, or rclone.
