#!/usr/bin/env bash
set -euo pipefail

if (( $# != 1 )); then
  printf 'Usage: %s <release-directory>\n' "$0" >&2
  exit 2
fi

RELEASE_DIRECTORY=$(CDPATH='' cd -- "$1" && pwd)
ARCHIVE="$RELEASE_DIRECTORY/deck-snapshot-linux-amd64.tar.gz"
CHECKSUM="$RELEASE_DIRECTORY/deck-snapshot-linux-amd64.sha256"
INSTALLER="$RELEASE_DIRECTORY/deck_snapshot_installer.desktop"
for path in "$ARCHIVE" "$CHECKSUM" "$INSTALLER"; do
  test -f "$path"
  test ! -L "$path"
done

TEST_ROOT=$(mktemp -d)
LOCK_HOLDER_PID=
cleanup_test() {
  if [[ -n "$LOCK_HOLDER_PID" ]]; then
    kill "$LOCK_HOLDER_PID" 2>/dev/null || true
    wait "$LOCK_HOLDER_PID" 2>/dev/null || true
  fi
  rm -rf -- "$TEST_ROOT"
}
trap cleanup_test EXIT
PAYLOAD=$(sed -n 's|^Exec=/usr/bin/env bash -c "echo \([^[:space:]]*\) .*|\1|p' "$INSTALLER")
test -n "$PAYLOAD"
printf '%s' "$PAYLOAD" | base64 -d >"$TEST_ROOT/bootstrap.sh"
bash -n "$TEST_ROOT/bootstrap.sh"
grep -Fx "BASE_URL=\"https://github.com/Fizzywood/deck-snapshot-releases/releases/download/\$VERSION\"" "$TEST_ROOT/bootstrap.sh" >/dev/null

FAKE_BIN="$TEST_ROOT/fake-bin"
mkdir -m 0700 -- "$FAKE_BIN"
cat >"$FAKE_BIN/curl" <<'EOF'
#!/usr/bin/env bash
exit 95
EOF
cat >"$FAKE_BIN/kdialog" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$DECK_SNAPSHOT_TEST_KDIALOG_LOG"
if [[ " $* " == *' --msgbox '* ]]; then
  exit 80
fi
EOF
cat >"$FAKE_BIN/xdg-user-dir" <<'EOF'
#!/usr/bin/env bash
test "${1:-}" = DOWNLOAD
printf '%s\n' "$HOME/Downloads"
EOF
chmod 0755 -- "$FAKE_BIN/curl" "$FAKE_BIN/kdialog" "$FAKE_BIN/xdg-user-dir"

run_local_install() {
  local test_home=$1
  mkdir -p -- "$test_home/Downloads" "$test_home/.local/share"
  install -m 0600 -- "$ARCHIVE" "$test_home/Downloads/deck-snapshot-linux-amd64.tar.gz"
  install -m 0600 -- "$CHECKSUM" "$test_home/Downloads/deck-snapshot-linux-amd64.sha256"
  DECK_SNAPSHOT_TEST_KDIALOG_LOG="$test_home/kdialog.log" \
    HOME="$test_home" \
    XDG_DATA_HOME="$test_home/.local/share" \
    PATH="$FAKE_BIN:$PATH" \
    bash "$TEST_ROOT/bootstrap.sh"
}

VALID_HOME="$TEST_ROOT/valid-home"
run_local_install "$VALID_HOME"
test -x "$VALID_HOME/.local/lib/deck-snapshot/deck-snapshot"
test -x "$VALID_HOME/.local/lib/deck-snapshot/rclone"
test -x "$VALID_HOME/.local/lib/deck-snapshot/xdg-open"
test -f "$VALID_HOME/.local/share/applications/deck-snapshot.desktop"
test -f "$VALID_HOME/.local/share/icons/hicolor/scalable/apps/deck-snapshot.svg"
grep -Fx 'Icon=deck-snapshot' "$VALID_HOME/.local/share/applications/deck-snapshot.desktop" >/dev/null
grep -F 'was installed successfully' "$VALID_HOME/kdialog.log" >/dev/null
if grep -F 'could not be downloaded, verified, or installed' "$VALID_HOME/kdialog.log" >/dev/null; then
  printf 'Bootstrap reported an installation failure after installation succeeded.\n' >&2
  exit 1
fi

INVALID_HOME="$TEST_ROOT/invalid-home"
mkdir -p -- "$INVALID_HOME/Downloads" "$INVALID_HOME/.local/share"
install -m 0600 -- "$ARCHIVE" "$INVALID_HOME/Downloads/deck-snapshot-linux-amd64.tar.gz"
printf '%064d  deck-snapshot-linux-amd64.tar.gz\n' 0 >"$INVALID_HOME/Downloads/deck-snapshot-linux-amd64.sha256"
if DECK_SNAPSHOT_TEST_KDIALOG_LOG="$INVALID_HOME/kdialog.log" \
  HOME="$INVALID_HOME" \
  XDG_DATA_HOME="$INVALID_HOME/.local/share" \
  PATH="$FAKE_BIN:$PATH" \
  bash "$TEST_ROOT/bootstrap.sh"; then
  printf 'Bootstrap accepted a checksum that did not match its embedded release digest.\n' >&2
  exit 1
fi
test ! -e "$INVALID_HOME/.local/lib/deck-snapshot/deck-snapshot"
grep -F 'could not be downloaded, verified, or installed' "$INVALID_HOME/kdialog.log" >/dev/null

LOCKED_HOME="$TEST_ROOT/locked-home"
LOCKED_RUNTIME="$LOCKED_HOME/runtime"
LOCK_DIRECTORY="$LOCKED_RUNTIME/deck-snapshot"
LOCK_FILE="$LOCK_DIRECTORY/installer.lock"
LOCK_READY="$LOCKED_HOME/lock-ready"
LOCK_RELEASE="$LOCKED_HOME/lock-release"
mkdir -p -- "$LOCKED_HOME/Downloads" "$LOCKED_HOME/.local/share" "$LOCK_DIRECTORY"
chmod 0700 -- "$LOCKED_RUNTIME" "$LOCK_DIRECTORY"
install -m 0600 -- "$ARCHIVE" "$LOCKED_HOME/Downloads/deck-snapshot-linux-amd64.tar.gz"
install -m 0600 -- "$CHECKSUM" "$LOCKED_HOME/Downloads/deck-snapshot-linux-amd64.sha256"
(
  exec 8>>"$LOCK_FILE"
  flock 8
  touch "$LOCK_READY"
  while [[ ! -e "$LOCK_RELEASE" ]]; do
    sleep 0.05
  done
) &
LOCK_HOLDER_PID=$!
for _ in {1..100}; do
  [[ -e "$LOCK_READY" ]] && break
  sleep 0.05
done
test -e "$LOCK_READY"
DECK_SNAPSHOT_TEST_KDIALOG_LOG="$LOCKED_HOME/kdialog.log" \
  HOME="$LOCKED_HOME" \
  XDG_DATA_HOME="$LOCKED_HOME/.local/share" \
  XDG_RUNTIME_DIR="$LOCKED_RUNTIME" \
  PATH="$FAKE_BIN:$PATH" \
  bash "$TEST_ROOT/bootstrap.sh"
test ! -e "$LOCKED_HOME/.local/lib/deck-snapshot/deck-snapshot"
grep -F 'Another Deck Snapshot installer is already running' "$LOCKED_HOME/kdialog.log" >/dev/null
touch "$LOCK_RELEASE"
wait "$LOCK_HOLDER_PID"
LOCK_HOLDER_PID=

printf 'Bootstrap local-release install test passed.\n'
