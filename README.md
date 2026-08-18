# Deck Snapshot Source — v0.1.8

This public repository contains clean source snapshots corresponding to published Deck Snapshot releases. The `v0.1.8` tag matches the same version in the [official download repository](https://github.com/Fizzywood/deck-snapshot-releases/releases/tag/v0.1.8).

**[Download Deck Snapshot](https://github.com/Fizzywood/deck-snapshot-releases/releases/latest)**

Deck Snapshot is a local-first SteamOS tool for backing up and safely restoring supported Decky plugin state, CSS Loader customization, and custom Steam artwork. It is not a system image or a general-purpose backup tool.

## Build and test

Go 1.26 or later in the 1.26 release line is required.

```text
gofmt -w ./cmd ./internal ./tests
go vet ./...
go test ./...
go test -race ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o deck-snapshot-linux-amd64 ./cmd/deck-snapshot
```

The release binary additionally receives Google's non-confidential Desktop installed-app credential through protected release configuration. The credential is required by Google for the installed-app token exchange, is extractable from the public binary by design, and is never stored in this source snapshot or published as a separate file. New connections request exactly `drive.file` and `drive.appdata`.

## Repository boundaries

This is a release-source snapshot, not the private development repository. It intentionally excludes private history, issues, pull requests, internal coordination, hardware evidence, credentials, tokens, recovery material, real snapshots, and user data. The source, tests, security contract, packaging scripts, and user documentation needed to inspect and build the released product are included.

Distribution assets, checksums, installation instructions, and release notes are published at [Fizzywood/deck-snapshot-releases](https://github.com/Fizzywood/deck-snapshot-releases).

`SOURCE_VERSION` records the matching public release tag. `SOURCE_PROVENANCE` records that version and the exact private release commit used to generate this clean source snapshot, without exposing private Git history.

## Licensing

Deck Snapshot is source-available for transparency and inspection. It is all rights reserved and is not an open-source project. No permission to reuse, modify, or redistribute Deck Snapshot is granted. See [LICENSE](LICENSE). Third-party components retain their own licenses; see [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) and the applicable dependency metadata.

## AI-assisted development disclosure

Deck Snapshot was substantially designed and implemented with OpenAI Codex under human product ownership, a development approach sometimes called vibe coding. Releases are still subject to automated tests, focused security review, checksum verification, and real Steam Deck validation. AI assistance and testing do not guarantee that the software is error-free; keep independent copies of important data and review every restore plan.

Security-sensitive behavior is governed by [SECURITY.md](SECURITY.md). Do not publish credentials, recovery material, snapshot contents, or private paths in a public issue.

Deck Snapshot is an independent project and is not affiliated with or endorsed by Valve, Steam, Decky Loader, CSS Loader, Google, or rclone.
