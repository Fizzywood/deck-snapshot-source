#!/usr/bin/env bash
set -euo pipefail

if (( $# != 4 )); then
  printf 'Usage: %s <version> <deck-snapshot-binary> <rclone-binary> <output-directory>\n' "$0" >&2
  exit 2
fi

VERSION=$1
DECK_SNAPSHOT_BINARY=$2
RCLONE_BINARY=$3
OUTPUT_DIRECTORY=$4
REPOSITORY_ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)

if [[ ! "$VERSION" =~ ^v0\.1\.[0-7](-rc\.[1-9][0-9]*|-dev\.[0-9a-f]{40})?$ ]]; then
  printf 'Unsupported release version: %s\n' "$VERSION" >&2
  exit 2
fi
for source in "$DECK_SNAPSHOT_BINARY" "$RCLONE_BINARY"; do
  if [[ ! -f "$source" || -L "$source" ]]; then
    printf 'Release input is missing or unsafe: %s\n' "$source" >&2
    exit 1
  fi
done
if ! timeout --foreground 30s "$RCLONE_BINARY" version | awk 'NR == 1 { valid = ($0 == "rclone v1.74.4") } END { exit(valid ? 0 : 1) }'; then
  printf 'The release requires exactly rclone v1.74.4.\n' >&2
  exit 1
fi

mkdir -p -- "$OUTPUT_DIRECTORY"
OUTPUT_DIRECTORY=$(CDPATH='' cd -- "$OUTPUT_DIRECTORY" && pwd)
WORK_DIRECTORY=$(mktemp -d)
trap 'rm -rf -- "$WORK_DIRECTORY"' EXIT
PACKAGE_ROOT="$WORK_DIRECTORY/deck-snapshot"
mkdir -p -- "$PACKAGE_ROOT"

install -m 0755 -- "$DECK_SNAPSHOT_BINARY" "$PACKAGE_ROOT/deck-snapshot"
install -m 0755 -- "$RCLONE_BINARY" "$PACKAGE_ROOT/rclone"
install -m 0755 -- "$REPOSITORY_ROOT/scripts/deck-snapshot-ui" "$PACKAGE_ROOT/deck-snapshot-ui"
install -m 0755 -- "$REPOSITORY_ROOT/scripts/install.sh" "$PACKAGE_ROOT/install.sh"
install -m 0755 -- "$REPOSITORY_ROOT/scripts/uninstall.sh" "$PACKAGE_ROOT/uninstall.sh"
install -m 0644 -- "$REPOSITORY_ROOT/packaging/deck-snapshot.desktop" "$PACKAGE_ROOT/deck-snapshot.desktop"
install -m 0644 -- "$REPOSITORY_ROOT/packaging/deck-snapshot.svg" "$PACKAGE_ROOT/deck-snapshot.svg"
install -m 0644 -- "$REPOSITORY_ROOT/README.md" "$PACKAGE_ROOT/README.md"
install -m 0644 -- "$REPOSITORY_ROOT/SECURITY.md" "$PACKAGE_ROOT/SECURITY.md"
install -m 0644 -- "$REPOSITORY_ROOT/THIRD_PARTY_NOTICES.md" "$PACKAGE_ROOT/THIRD_PARTY_NOTICES.md"

ARCHIVE="$OUTPUT_DIRECTORY/deck-snapshot-linux-amd64.tar.gz"
CHECKSUM="$OUTPUT_DIRECTORY/deck-snapshot-linux-amd64.sha256"
INSTALLER="$OUTPUT_DIRECTORY/deck_snapshot_installer.desktop"
if tar --version 2>/dev/null | grep -q 'GNU tar'; then
  tar --sort=name --mtime='UTC 1970-01-01' --owner=0 --group=0 --numeric-owner -C "$WORK_DIRECTORY" -czf "$ARCHIVE" deck-snapshot
else
  tar -C "$WORK_DIRECTORY" -czf "$ARCHIVE" deck-snapshot
fi
(
  cd -- "$OUTPUT_DIRECTORY"
  sha256sum deck-snapshot-linux-amd64.tar.gz >deck-snapshot-linux-amd64.sha256
)
"$REPOSITORY_ROOT/scripts/package-bootstrap-installer.sh" "$VERSION" "$ARCHIVE" "$CHECKSUM" "$INSTALLER"

test "$(wc -l <"$CHECKSUM")" -eq 1
printf 'Release package created: %s\n' "$ARCHIVE"
