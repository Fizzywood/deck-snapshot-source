#!/usr/bin/env bash
set -euo pipefail

SOURCE_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
DATA_HOME=${XDG_DATA_HOME:-"$HOME/.local/share"}
if [[ "$DATA_HOME" != /* || "$DATA_HOME" == *['|&"']* || "$DATA_HOME" == *$'\n'* ]]; then
  printf 'XDG data path is not safe for desktop installation.\n' >&2
  exit 1
fi
APP_DIR=$(dirname -- "$DATA_HOME")/lib/deck-snapshot
APPLICATIONS_DIR=$DATA_HOME/applications
LAUNCHER="$APPLICATIONS_DIR/deck-snapshot.desktop"
ICON_THEME_DIR="$DATA_HOME/icons/hicolor"
ICONS_DIR="$ICON_THEME_DIR/scalable/apps"
ICON="$ICONS_DIR/deck-snapshot.svg"

if [[ -L "$LAUNCHER" ]]; then
  printf 'Refusing to replace a symbolic-link launcher: %s\n' "$LAUNCHER" >&2
  exit 1
fi

for source in deck-snapshot rclone deck-snapshot-ui uninstall.sh deck-snapshot.desktop deck-snapshot.svg; do
  if [[ ! -f "$SOURCE_DIR/$source" || -L "$SOURCE_DIR/$source" ]]; then
    printf 'Installation source is missing or unsafe: %s\n' "$source" >&2
    exit 1
  fi
done

for directory in "$(dirname -- "$APP_DIR")" "$APPLICATIONS_DIR" "$DATA_HOME/icons" "$ICON_THEME_DIR" "$ICON_THEME_DIR/scalable" "$ICONS_DIR"; do
	if [[ -L "$directory" ]]; then
    printf 'Refusing to install through a symbolic link: %s\n' "$directory" >&2
    exit 1
	fi
	mkdir -p -- "$directory"
	chmod 755 -- "$directory"
done
if [[ -L "$APP_DIR" ]]; then
  printf 'Refusing to install through a symbolic link: %s\n' "$APP_DIR" >&2
  exit 1
fi
mkdir -p -- "$APP_DIR"
chmod 700 -- "$APP_DIR"

install_file() {
  local source=$1
  local target=$2
  local mode=$3
  local temporary
  if [[ -L "$target" ]]; then
    printf 'Refusing to replace a symbolic link: %s\n' "$target" >&2
    exit 1
  fi
  temporary=$(mktemp --tmpdir="$(dirname -- "$target")" .deck-snapshot-install.XXXXXX)
  install -m "$mode" -- "$source" "$temporary"
  mv -f -- "$temporary" "$target"
}

install_file "$SOURCE_DIR/deck-snapshot" "$APP_DIR/deck-snapshot" 0755
install_file "$SOURCE_DIR/rclone" "$APP_DIR/rclone" 0755
install_file "$SOURCE_DIR/deck-snapshot" "$APP_DIR/xdg-open" 0755
install_file "$SOURCE_DIR/deck-snapshot-ui" "$APP_DIR/deck-snapshot-ui" 0755
install_file "$SOURCE_DIR/uninstall.sh" "$APP_DIR/uninstall.sh" 0755
install_file "$SOURCE_DIR/deck-snapshot.svg" "$ICON" 0644

launcher_tmp=$(mktemp --tmpdir="$APPLICATIONS_DIR" .deck-snapshot-launcher.XXXXXX)
sed "s|@APP_DIR@|$APP_DIR|g" "$SOURCE_DIR/deck-snapshot.desktop" >"$launcher_tmp"
chmod 0644 -- "$launcher_tmp"
mv -f -- "$launcher_tmp" "$LAUNCHER"

if command -v update-desktop-database >/dev/null 2>&1; then
  update-desktop-database "$APPLICATIONS_DIR" >/dev/null 2>&1 || true
fi
if command -v gtk-update-icon-cache >/dev/null 2>&1; then
  gtk-update-icon-cache --force --ignore-theme-index "$ICON_THEME_DIR" >/dev/null 2>&1 || true
fi

printf 'Deck Snapshot was installed for this user.\nLauncher: %s\n' "$LAUNCHER"
