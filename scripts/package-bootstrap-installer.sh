#!/usr/bin/env bash
set -euo pipefail

if (( $# != 4 )); then
  printf 'Usage: %s <version> <archive> <checksum> <installer-output>\n' "$0" >&2
  exit 2
fi

VERSION=$1
ARCHIVE=$2
CHECKSUM=$3
INSTALLER=$4
REPOSITORY_ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)

if [[ ! "$VERSION" =~ ^v0\.1\.[0-8](-rc\.[1-9][0-9]*|-dev\.[0-9a-f]{40})?$ ]]; then
  printf 'Unsupported installer version: %s\n' "$VERSION" >&2
  exit 2
fi
for source in "$ARCHIVE" "$CHECKSUM"; do
  if [[ ! -f "$source" || -L "$source" ]]; then
    printf 'Installer input is missing or unsafe: %s\n' "$source" >&2
    exit 1
  fi
done

ARCHIVE=$(CDPATH='' cd -- "$(dirname -- "$ARCHIVE")" && pwd)/$(basename -- "$ARCHIVE")
CHECKSUM=$(CDPATH='' cd -- "$(dirname -- "$CHECKSUM")" && pwd)/$(basename -- "$CHECKSUM")
test "$(basename -- "$ARCHIVE")" = deck-snapshot-linux-amd64.tar.gz
test "$(wc -l <"$CHECKSUM")" -eq 1
grep -Eq '^[0-9a-f]{64}  deck-snapshot-linux-amd64\.tar\.gz$' "$CHECKSUM"
ARCHIVE_SHA256=$(sha256sum "$ARCHIVE" | awk '{print $1}')
grep -Fx "$ARCHIVE_SHA256  deck-snapshot-linux-amd64.tar.gz" "$CHECKSUM" >/dev/null

OUTPUT_DIRECTORY=$(dirname -- "$INSTALLER")
mkdir -p -- "$OUTPUT_DIRECTORY"
OUTPUT_DIRECTORY=$(CDPATH='' cd -- "$OUTPUT_DIRECTORY" && pwd)
INSTALLER="$OUTPUT_DIRECTORY/$(basename -- "$INSTALLER")"
WORK_DIRECTORY=$(mktemp -d)
trap 'rm -rf -- "$WORK_DIRECTORY"' EXIT
BOOTSTRAP="$WORK_DIRECTORY/bootstrap-installer.sh"

sed -e "s|@VERSION@|$VERSION|g" -e "s|@ARCHIVE_SHA256@|$ARCHIVE_SHA256|g" "$REPOSITORY_ROOT/scripts/bootstrap-installer.sh.in" >"$BOOTSTRAP"
bash -n "$BOOTSTRAP"
PAYLOAD=$(base64 <"$BOOTSTRAP" | tr -d '\n')
sed -e "s|@VERSION@|$VERSION|g" -e "s|@PAYLOAD@|$PAYLOAD|g" "$REPOSITORY_ROOT/packaging/deck_snapshot_installer.desktop.in" >"$INSTALLER"
chmod 0755 -- "$INSTALLER"
