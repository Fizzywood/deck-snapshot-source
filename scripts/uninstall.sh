#!/usr/bin/env bash
set -euo pipefail

DATA_HOME=${XDG_DATA_HOME:-"$HOME/.local/share"}
if [[ "$DATA_HOME" != /* || "$DATA_HOME" == *$'\n'* ]]; then
  printf 'XDG data path is not safe for uninstall.\n' >&2
  exit 1
fi
APP_DIR=$(dirname -- "$DATA_HOME")/lib/deck-snapshot
APPLICATIONS_DIR=$DATA_HOME/applications
LAUNCHER="$APPLICATIONS_DIR/deck-snapshot.desktop"
ICON_THEME_DIR="$DATA_HOME/icons/hicolor"
ICON="$ICON_THEME_DIR/scalable/apps/deck-snapshot.svg"

remove_file() {
  local target=$1
  if [[ -e "$target" || -L "$target" ]]; then
    rm -f -- "$target"
  fi
}

remove_file "$LAUNCHER"
remove_file "$ICON"
remove_file "$APP_DIR/deck-snapshot"
remove_file "$APP_DIR/rclone"
remove_file "$APP_DIR/xdg-open"
remove_file "$APP_DIR/deck-snapshot-ui"
remove_file "$APP_DIR/deck-snapshot.desktop"
remove_file "$APP_DIR/uninstall.sh"
rmdir -- "$APP_DIR" 2>/dev/null || true

if command -v update-desktop-database >/dev/null 2>&1; then
  update-desktop-database "$APPLICATIONS_DIR" >/dev/null 2>&1 || true
fi
if command -v gtk-update-icon-cache >/dev/null 2>&1; then
  gtk-update-icon-cache --force --ignore-theme-index "$ICON_THEME_DIR" >/dev/null 2>&1 || true
fi

printf 'Deck Snapshot application files were removed.\n'
printf 'Local snapshots, recovery snapshots, reports, cloud configuration, and recovery material were preserved.\n'
