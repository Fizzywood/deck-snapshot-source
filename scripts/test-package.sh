#!/usr/bin/env bash
set -euo pipefail

if (( $# != 1 )); then
  printf 'Usage: %s <deck-snapshot-linux-amd64.tar.gz>\n' "$0" >&2
  exit 2
fi

ARCHIVE=$1
if [[ ! -f "$ARCHIVE" || -L "$ARCHIVE" ]]; then
  printf 'Package archive is missing or unsafe.\n' >&2
  exit 1
fi

TEST_ROOT=$(mktemp -d)
trap 'rm -rf -- "$TEST_ROOT"' EXIT
tar -xzf "$ARCHIVE" -C "$TEST_ROOT"
PACKAGE="$TEST_ROOT/deck-snapshot"
for path in deck-snapshot rclone deck-snapshot-ui install.sh uninstall.sh deck-snapshot.desktop deck-snapshot.svg README.md SECURITY.md THIRD_PARTY_NOTICES.md; do
  test -f "$PACKAGE/$path"
  test ! -L "$PACKAGE/$path"
done
grep -F '<svg ' "$PACKAGE/deck-snapshot.svg" >/dev/null
if grep -F '<text' "$PACKAGE/deck-snapshot.svg" >/dev/null; then
  printf 'The application icon contains rendered text.\n' >&2
  exit 1
fi

export HOME="$TEST_ROOT/home"
export XDG_DATA_HOME="$HOME/.local/share"
mkdir -p -- "$HOME" "$XDG_DATA_HOME"
bash "$PACKAGE/install.sh"
APP_DIR="$HOME/.local/lib/deck-snapshot"
test -x "$APP_DIR/deck-snapshot"
test -x "$APP_DIR/rclone"
test -x "$APP_DIR/xdg-open"
cmp -s -- "$APP_DIR/deck-snapshot" "$APP_DIR/xdg-open"
if "$APP_DIR/xdg-open" https://example.com/; then
  printf 'Installed browser adapter accepted a non-Google URL.\n' >&2
  exit 1
fi
test -x "$APP_DIR/deck-snapshot-ui"
test -f "$XDG_DATA_HOME/applications/deck-snapshot.desktop"
test -f "$XDG_DATA_HOME/icons/hicolor/scalable/apps/deck-snapshot.svg"
test ! -L "$XDG_DATA_HOME/icons/hicolor/scalable/apps/deck-snapshot.svg"
cmp -s -- "$PACKAGE/deck-snapshot.svg" "$XDG_DATA_HOME/icons/hicolor/scalable/apps/deck-snapshot.svg"
grep -F "Exec=\"$APP_DIR/deck-snapshot-ui\"" "$XDG_DATA_HOME/applications/deck-snapshot.desktop" >/dev/null
grep -Fx 'Icon=deck-snapshot' "$XDG_DATA_HOME/applications/deck-snapshot.desktop" >/dev/null
if command -v desktop-file-validate >/dev/null 2>&1; then
  desktop-file-validate "$XDG_DATA_HOME/applications/deck-snapshot.desktop"
fi

printf 'preserve-owned-extra\n' >"$APP_DIR/user-marker"
bash "$PACKAGE/install.sh"
test -f "$APP_DIR/user-marker"
test -x "$APP_DIR/deck-snapshot"
test -x "$APP_DIR/rclone"
test -x "$APP_DIR/xdg-open"

mkdir -p -- "$XDG_DATA_HOME/deck-snapshot/snapshots"
printf 'preserve-me\n' >"$XDG_DATA_HOME/deck-snapshot/snapshots/local-snapshot-marker"
printf '<svg/>\n' >"$XDG_DATA_HOME/icons/hicolor/scalable/apps/unrelated-user-icon.svg"
bash "$APP_DIR/uninstall.sh"
test ! -e "$APP_DIR/deck-snapshot"
test ! -e "$APP_DIR/xdg-open"
test ! -e "$XDG_DATA_HOME/applications/deck-snapshot.desktop"
test ! -e "$XDG_DATA_HOME/icons/hicolor/scalable/apps/deck-snapshot.svg"
test -f "$XDG_DATA_HOME/icons/hicolor/scalable/apps/unrelated-user-icon.svg"
test -f "$APP_DIR/user-marker"
test -f "$XDG_DATA_HOME/deck-snapshot/snapshots/local-snapshot-marker"

printf 'Package install/uninstall test passed.\n'
