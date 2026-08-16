#!/usr/bin/env bash
set -euo pipefail

REPOSITORY_ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
TEST_ROOT=$(mktemp -d)
trap 'rm -rf -- "$TEST_ROOT"' EXIT
APP_DIR="$TEST_ROOT/app"
HOME_DIR="$TEST_ROOT/home"
RUNTIME_DIR="$TEST_ROOT/runtime"
mkdir -p -- "$APP_DIR" "$HOME_DIR" "$RUNTIME_DIR"
chmod 0700 -- "$RUNTIME_DIR"
install -m 0755 -- "$REPOSITORY_ROOT/scripts/deck-snapshot-ui" "$APP_DIR/deck-snapshot-ui"
touch "$TEST_ROOT/recovery.json" "$TEST_ROOT/snapshot.tar.gz"

cat >"$APP_DIR/deck-snapshot" <<'SCRIPT'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$TEST_LOG"
if [[ "$*" == "version" ]]; then
  printf 'Deck Snapshot v0.1.3-dev.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n'
  exit 0
fi
if [[ "$*" == "doctor" ]]; then
  printf 'Deck Snapshot diagnostics: ready\nSteam detected: true\nDecky detected: true\nCloud configured: false\nSteam accounts: 1\nPlugins: 1\nCSS themes/profiles: 1\nCustom artwork: 1\nCandidate files: 4\nDiscovery warnings: 0\nChecked at: 2026-08-15T12:24:00Z\n'
  exit 0
fi
if [[ "$*" == "settings show" && "${UI_SCENARIO:-}" =~ ^(dashboard|backup_cloud_retry|backup_cloud_fail|cloud)$ ]]; then
  printf 'Automatic cloud upload: true\nCloud recovery file: %s\n' "$TEST_RECOVERY_FILE"
  exit 0
fi
if [[ "$*" == "snapshot create" && "${UI_SCENARIO:-}" =~ ^backup_cloud_ ]]; then
  printf 'Snapshot created and validated: %s\n' "$SNAPSHOT_FILE"
  exit 0
fi
if [[ "$*" == "cloud upload --recovery-file $TEST_RECOVERY_FILE $SNAPSHOT_FILE" && "${UI_SCENARIO:-}" =~ ^backup_cloud_ ]]; then
  count=0
  if [[ -f "$UPLOAD_COUNT" ]]; then
    count=$(<"$UPLOAD_COUNT")
  fi
  count=$((count + 1))
  printf '%s\n' "$count" >"$UPLOAD_COUNT"
  if [[ "$UI_SCENARIO" == "backup_cloud_fail" || ( "$UI_SCENARIO" == "backup_cloud_retry" && "$count" == 1 ) ]]; then
    printf 'Synthetic redacted upload failure.\n' >&2
    exit 1
  fi
  printf 'Protected snapshot uploaded and byte-verified.\n'
  exit 0
fi
if [[ "$*" == "cloud status --recovery-file $TEST_RECOVERY_FILE" && "${UI_SCENARIO:-}" == "dashboard" ]]; then
  printf 'Cloud connected: true\nClient-side protection: true\nRecovery acknowledged: true\nGoogle Drive scope: drive.file\nSnapshot folder: My Drive/Deck Snapshot/Snapshots\nLegacy migration source: false\n'
  exit 0
fi
if [[ "$*" == "cloud list --recovery-file $TEST_RECOVERY_FILE" && "${UI_SCENARIO:-}" == "dashboard" ]]; then
  printf 'deck-snapshot-20260815T010203Z-dsnap-dashboard.tar.gz  42 bytes  2026-08-15T01:02:03Z\n'
  exit 0
fi
if [[ "$*" == "snapshot inspect $SNAPSHOT_FILE" ]]; then
  printf 'Snapshot: dsnap-dashboard\nCreated: 2026-08-15T01:02:03Z\nApplication version: v0.1.4-dev\nFiles: 4 (42 bytes)\nPlugins: 1\nCSS themes/profiles: 2\nArtwork: 3\nWarnings: 0\n'
  exit 0
fi
if [[ "${UI_SCENARIO:-}" =~ ^(restore|snapshot_restore)$ && "$*" == "restore plan $SNAPSHOT_FILE" ]]; then
  printf 'Restore plan created without target writes: %s\nPlan ID: restore-aaaaaaaaaaaaaaaa\nApproval hash: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\nActions: 1\nPlugins: 0\nBlocking: false\nRequired free space: 1 bytes\n' "$TEST_PLAN_FILE"
  exit 0
fi
if [[ "${UI_SCENARIO:-}" =~ ^(restore|snapshot_restore)$ && "$*" == "restore inspect --details $TEST_PLAN_FILE" ]]; then
  printf 'Plan ID: restore-aaaaaaaaaaaaaaaa\nFile action: unchanged | steam | fixture -> synthetic-target\n'
  exit 0
fi
printf 'Synthetic core success.\n'
SCRIPT
chmod 0755 -- "$APP_DIR/deck-snapshot"

cat >"$TEST_ROOT/kdialog" <<'SCRIPT'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$KDIALOG_LOG"
if [[ " $* " == *' --progressbar '* ]]; then
  printf 'org.kde.kdialog-4242 /ProgressDialog\n'
  exit 0
fi
if [[ " $* " == *' --menu '* ]]; then
  count=0
  if [[ -f "$MENU_COUNT" ]]; then
    count=$(<"$MENU_COUNT")
  fi
  count=$((count + 1))
  printf '%s\n' "$count" >"$MENU_COUNT"
  if [[ "$UI_SCENARIO" == local ]]; then
    case "$count" in
      1) printf 'create\n' ;;
      2) printf 'more\n' ;;
      3) printf 'doctor\n' ;;
      *) printf 'quit\n' ;;
    esac
  elif [[ "$UI_SCENARIO" == cloud ]]; then
    case "$count" in
      1) printf 'more\n' ;;
      2) printf 'cloud\n' ;;
      3) printf 'advanced\n' ;;
      4) printf 'upload\n' ;;
      *) printf 'quit\n' ;;
    esac
  elif [[ "$UI_SCENARIO" == reconnect ]]; then
    case "$count" in
      1) printf 'more\n' ;;
      2) printf 'cloud\n' ;;
      3) printf 'connect\n' ;;
      *) printf 'quit\n' ;;
    esac
  elif [[ "$UI_SCENARIO" == restore ]]; then
    case "$count" in
      1) printf 'restore\n' ;;
      2) printf 'local:%s\n' "${SNAPSHOT_FILE##*/}" ;;
      *) printf 'quit\n' ;;
    esac
  elif [[ "$UI_SCENARIO" == snapshots ]]; then
    case "$count" in
      1) printf 'snapshots\n' ;;
      2) printf 'local:%s\n' "${SNAPSHOT_FILE##*/}" ;;
      3) printf 'close\n' ;;
      *) printf 'quit\n' ;;
    esac
  elif [[ "$UI_SCENARIO" == snapshot_restore ]]; then
    case "$count" in
      1) printf 'snapshots\n' ;;
      2) printf 'local:%s\n' "${SNAPSHOT_FILE##*/}" ;;
      3) printf 'restore\n' ;;
      *) printf 'quit\n' ;;
    esac
  elif [[ "$UI_SCENARIO" == decky_missing ]]; then
    case "$count" in
      1) printf 'restore\n' ;;
      2) printf 'local:%s\n' "${SNAPSHOT_FILE##*/}" ;;
      3) printf 'get_decky\n' ;;
      4) printf 'check_again\n' ;;
      5) printf 'close\n' ;;
      *) printf 'quit\n' ;;
    esac
  elif [[ "$UI_SCENARIO" == settings ]]; then
    if (( count == 1 )); then printf 'settings\n'; else printf 'quit\n'; fi
  elif [[ "$UI_SCENARIO" == locked_disconnect ]]; then
    case "$count" in
      1) printf 'more\n' ;;
      2) printf 'cloud\n' ;;
      3) printf 'advanced\n' ;;
      4) printf 'disconnect\n' ;;
      *) printf 'quit\n' ;;
    esac
  elif [[ "$UI_SCENARIO" =~ ^backup_cloud_ || "$UI_SCENARIO" == backup_locked ]]; then
    if (( count == 1 )); then printf 'create\n'; else printf 'quit\n'; fi
  else
    printf 'quit\n'
  fi
  exit 0
fi
if [[ "$UI_SCENARIO" == backup_cloud_fail && " $* " == *' --warningyesno '* ]]; then
  exit 1
fi
if [[ "$UI_SCENARIO" == backup_cloud_fail && " $* " == *' --yesno '* ]]; then
  exit 1
fi
if [[ " $* " == *' --getopenfilename '* ]]; then
  count=0
  if [[ -f "$FILE_COUNT" ]]; then
    count=$(<"$FILE_COUNT")
  fi
  count=$((count + 1))
  printf '%s\n' "$count" >"$FILE_COUNT"
  if [[ "$UI_SCENARIO" == reconnect || "$UI_SCENARIO" == locked_disconnect ]]; then
    printf '%s\n' "$TEST_RECOVERY_FILE"
  elif (( count == 1 )); then
    printf '%s\n' "$SNAPSHOT_FILE"
  else
    printf '%s\n' "$TEST_RECOVERY_FILE"
  fi
  exit 0
fi
if [[ " $* " == *' --password '* ]]; then
  count=0
  if [[ -f "$PASSWORD_COUNT" ]]; then
    count=$(<"$PASSWORD_COUNT")
  fi
  count=$((count + 1))
  printf '%s\n' "$count" >"$PASSWORD_COUNT"
  printf 'configuration-password\n'
  exit 0
fi
exit 0
SCRIPT
chmod 0755 -- "$TEST_ROOT/kdialog"

cat >"$TEST_ROOT/qdbus6" <<'SCRIPT'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$QDBUS_LOG"
SCRIPT
chmod 0755 -- "$TEST_ROOT/qdbus6"

cat >"$TEST_ROOT/flock" <<'SCRIPT'
#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" == "-n" && "${UI_SCENARIO:-}" == "backup_locked" ]]; then
  exit 1
fi
exit 0
SCRIPT
chmod 0755 -- "$TEST_ROOT/flock"

cat >"$TEST_ROOT/xdg-open" <<'SCRIPT'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$BROWSER_LOG"
SCRIPT
chmod 0755 -- "$TEST_ROOT/xdg-open"

export TEST_LOG="$TEST_ROOT/core.log"
export MENU_COUNT="$TEST_ROOT/menu.count"
export FILE_COUNT="$TEST_ROOT/file.count"
export PASSWORD_COUNT="$TEST_ROOT/password.count"
export UPLOAD_COUNT="$TEST_ROOT/upload.count"
export KDIALOG_LOG="$TEST_ROOT/kdialog.log"
export QDBUS_LOG="$TEST_ROOT/qdbus.log"
export BROWSER_LOG="$TEST_ROOT/browser.log"
export SNAPSHOT_FILE="$TEST_ROOT/snapshot.tar.gz"
export TEST_RECOVERY_FILE="$TEST_ROOT/recovery.json"
export TEST_PLAN_FILE="$TEST_ROOT/restore-plan.json"
export HOME="$HOME_DIR"
export XDG_RUNTIME_DIR="$RUNTIME_DIR"
export KDIALOG="$TEST_ROOT/kdialog"
export PATH="$TEST_ROOT:$PATH"

export UI_SCENARIO=local
"$APP_DIR/deck-snapshot-ui"
grep -Fx 'snapshot create' "$TEST_LOG" >/dev/null
grep -Fx 'doctor' "$TEST_LOG" >/dev/null
grep -F -- '--icon deck-snapshot' "$KDIALOG_LOG" >/dev/null
grep -F 'Backup complete' "$KDIALOG_LOG" >/dev/null
grep -F 'Steam               ✓ Ready' "$KDIALOG_LOG" >/dev/null
grep -F -- '--progressbar Creating and validating the local backup… 0' "$KDIALOG_LOG" >/dev/null
grep -F 'org.kde.kdialog-4242 /ProgressDialog close' "$QDBUS_LOG" >/dev/null
grep -F 'Diagnostics' "$KDIALOG_LOG" >/dev/null

: >"$TEST_LOG"
: >"$KDIALOG_LOG"
: >"$QDBUS_LOG"
rm -f -- "$MENU_COUNT" "$UPLOAD_COUNT"
mkdir -p -- "$HOME/.config/deck-snapshot/cloud"
touch "$HOME/.config/deck-snapshot/cloud/rclone.conf" "$HOME/.config/deck-snapshot/cloud/config-password"
export UI_SCENARIO=backup_cloud_retry
"$APP_DIR/deck-snapshot-ui"
test "$(grep -Fc "cloud upload --recovery-file $TEST_RECOVERY_FILE $SNAPSHOT_FILE" "$TEST_LOG")" -eq 2
grep -F 'Retrying the protected Google Drive upload…' "$KDIALOG_LOG" >/dev/null
grep -F 'Local              ✓ Saved' "$KDIALOG_LOG" >/dev/null
grep -F 'Google Drive       ✓ Stored' "$KDIALOG_LOG" >/dev/null

: >"$TEST_LOG"
: >"$KDIALOG_LOG"
rm -f -- "$MENU_COUNT" "$UPLOAD_COUNT"
export UI_SCENARIO=backup_cloud_fail
"$APP_DIR/deck-snapshot-ui"
test "$(grep -Fc "cloud upload --recovery-file $TEST_RECOVERY_FILE $SNAPSHOT_FILE" "$TEST_LOG")" -eq 1
grep -F 'The local backup is safe. Retry the Google Drive upload now?' "$KDIALOG_LOG" >/dev/null
grep -F 'Backup created locally' "$KDIALOG_LOG" >/dev/null
grep -F 'Local              ✓ Saved' "$KDIALOG_LOG" >/dev/null
grep -F 'Google Drive       ! Upload failed' "$KDIALOG_LOG" >/dev/null

: >"$TEST_LOG"
: >"$KDIALOG_LOG"
rm -f -- "$MENU_COUNT"
mkdir -p -- "$XDG_RUNTIME_DIR/deck-snapshot"
chmod 0700 -- "$XDG_RUNTIME_DIR/deck-snapshot"
export UI_SCENARIO=backup_locked
"$APP_DIR/deck-snapshot-ui"
if grep -Fx 'snapshot create' "$TEST_LOG" >/dev/null; then
  printf 'A duplicate backup started while the backup lock was held.\n' >&2
  exit 1
fi
grep -F 'Another backup is already running' "$KDIALOG_LOG" >/dev/null

: >"$TEST_LOG"
rm -f -- "$MENU_COUNT" "$FILE_COUNT" "$PASSWORD_COUNT"
export UI_SCENARIO=cloud
"$APP_DIR/deck-snapshot-ui"
grep -Fx "cloud upload --recovery-file $TEST_RECOVERY_FILE $SNAPSHOT_FILE" "$TEST_LOG" >/dev/null
test ! -e "$PASSWORD_COUNT"

: >"$TEST_LOG"
: >"$KDIALOG_LOG"
: >"$BROWSER_LOG"
rm -f -- "$MENU_COUNT" "$FILE_COUNT" "$PASSWORD_COUNT"
mkdir -p -- "$HOME/.local/share/deck-snapshot/snapshots"
export SNAPSHOT_FILE="$HOME/.local/share/deck-snapshot/snapshots/deck-snapshot-20260815T010203Z-dsnap-dashboard.tar.gz"
touch "$SNAPSHOT_FILE"
export UI_SCENARIO=decky_missing
"$APP_DIR/deck-snapshot-ui"
grep -F 'Decky</td><td align="right">— Not installed</td>' "$KDIALOG_LOG" >/dev/null
grep -F 'Decky Loader — Not installed' "$KDIALOG_LOG" >/dev/null
grep -F 'Decky Loader is required to restore Decky plugins and their settings.' "$KDIALOG_LOG" >/dev/null
grep -Fx 'https://github.com/SteamDeckHomebrew/decky-loader' "$BROWSER_LOG" >/dev/null
if grep -F 'restore plan ' "$TEST_LOG" >/dev/null; then
  printf 'Decky recovery prompt unexpectedly started restore planning.\n' >&2
  exit 1
fi

: >"$TEST_LOG"
: >"$KDIALOG_LOG"
rm -f -- "$MENU_COUNT" "$FILE_COUNT" "$PASSWORD_COUNT"
mkdir -p -- "$HOME/.config/deck-snapshot/cloud" "$HOME/.local/share/deck-snapshot/snapshots" "$HOME/homebrew/plugins/example-plugin"
touch "$HOME/.config/deck-snapshot/cloud/rclone.conf" "$HOME/.config/deck-snapshot/cloud/config-password"
touch "$SNAPSHOT_FILE"
export UI_SCENARIO=dashboard
"$APP_DIR/deck-snapshot-ui"
grep -F '<td align="left">Google Drive</td><td align="right">✓ Connected</td>' "$KDIALOG_LOG" >/dev/null
grep -F '<td align="left">Decky</td><td align="right">✓ 1 plugins</td>' "$KDIALOG_LOG" >/dev/null
grep -F '<td align="left">Stored</td><td align="right">✓ Local + Cloud</td>' "$KDIALOG_LOG" >/dev/null
if grep -F '<td align="left">Updates</td>' "$KDIALOG_LOG" >/dev/null; then
  printf 'The dashboard still exposed a permanent updates row.\n' >&2
  exit 1
fi
grep -F 'v0.1.3 development build' "$KDIALOG_LOG" >/dev/null
grep -F 'Create Backup' "$KDIALOG_LOG" >/dev/null
grep -F ' Restore ' "$KDIALOG_LOG" >/dev/null
grep -F ' Snapshots ' "$KDIALOG_LOG" >/dev/null
grep -F ' More options ' "$KDIALOG_LOG" >/dev/null
if grep -F ' doctor Diagnostics' "$KDIALOG_LOG" >/dev/null; then
  printf 'The primary dashboard still exposed Diagnostics.\n' >&2
  exit 1
fi
if grep -F 'Unlock a v0.1.0 cloud connection' "$KDIALOG_LOG" >/dev/null; then
  printf 'The primary dashboard exposed a migration action.\n' >&2
  exit 1
fi
grep -Fx "cloud status --recovery-file $TEST_RECOVERY_FILE" "$TEST_LOG" >/dev/null
grep -Fx "cloud list --recovery-file $TEST_RECOVERY_FILE" "$TEST_LOG" >/dev/null

: >"$TEST_LOG"
rm -f -- "$MENU_COUNT" "$FILE_COUNT" "$PASSWORD_COUNT"
export UI_SCENARIO=reconnect
"$APP_DIR/deck-snapshot-ui"
grep -Fx "cloud disconnect --recovery-file $TEST_RECOVERY_FILE" "$TEST_LOG" >/dev/null
grep -Fx "cloud connect --recovery-file $TEST_RECOVERY_FILE" "$TEST_LOG" >/dev/null

: >"$TEST_LOG"
rm -f -- "$MENU_COUNT" "$FILE_COUNT" "$PASSWORD_COUNT"
export UI_SCENARIO=snapshots
"$APP_DIR/deck-snapshot-ui"
grep -Fx "snapshot inspect $SNAPSHOT_FILE" "$TEST_LOG" >/dev/null
if grep -F 'restore plan ' "$TEST_LOG" >/dev/null; then
  printf 'Snapshot browsing unexpectedly started restore planning.\n' >&2
  exit 1
fi
grep -F 'Backup date/time: 2026-08-15T01:02:03Z' "$KDIALOG_LOG" >/dev/null
grep -F 'Validation: Valid' "$KDIALOG_LOG" >/dev/null
grep -F 'Decky plugins: 1' "$KDIALOG_LOG" >/dev/null
grep -F 'CSS Loader themes/profiles: 2' "$KDIALOG_LOG" >/dev/null
grep -F 'Steam artwork: 3' "$KDIALOG_LOG" >/dev/null
grep -F 'Restore this backup' "$KDIALOG_LOG" >/dev/null

: >"$TEST_LOG"
: >"$KDIALOG_LOG"
rm -f -- "$MENU_COUNT" "$FILE_COUNT" "$PASSWORD_COUNT"
touch "$TEST_PLAN_FILE"
export UI_SCENARIO=snapshot_restore
"$APP_DIR/deck-snapshot-ui"
test "$(grep -Fc "snapshot inspect $SNAPSHOT_FILE" "$TEST_LOG")" -eq 1
grep -Fx "restore plan $SNAPSHOT_FILE" "$TEST_LOG" >/dev/null
grep -Fx "restore inspect --details $TEST_PLAN_FILE" "$TEST_LOG" >/dev/null
grep -Fx "restore run --approve restore-aaaaaaaaaaaaaaaa --approval-hash aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa $TEST_PLAN_FILE" "$TEST_LOG" >/dev/null
if grep -F 'Deck Snapshot — Restore' "$KDIALOG_LOG" >/dev/null; then
  printf 'Snapshot restore unexpectedly opened a second snapshot chooser.\n' >&2
  exit 1
fi

: >"$TEST_LOG"
rm -f -- "$MENU_COUNT" "$FILE_COUNT" "$PASSWORD_COUNT"
export UI_SCENARIO=settings
"$APP_DIR/deck-snapshot-ui"
grep -Fx 'settings set --auto-upload true' "$TEST_LOG" >/dev/null

: >"$TEST_LOG"
rm -f -- "$MENU_COUNT" "$FILE_COUNT" "$PASSWORD_COUNT"
touch "$TEST_PLAN_FILE"
export UI_SCENARIO=restore
"$APP_DIR/deck-snapshot-ui"
grep -Fx "restore plan $SNAPSHOT_FILE" "$TEST_LOG" >/dev/null
grep -Fx "restore inspect --details $TEST_PLAN_FILE" "$TEST_LOG" >/dev/null
grep -Fx "restore run --approve restore-aaaaaaaaaaaaaaaa --approval-hash aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa $TEST_PLAN_FILE" "$TEST_LOG" >/dev/null

: >"$TEST_LOG"
rm -f -- "$MENU_COUNT" "$FILE_COUNT" "$PASSWORD_COUNT" "$HOME/.config/deck-snapshot/cloud/config-password"
export UI_SCENARIO=locked_disconnect
"$APP_DIR/deck-snapshot-ui"
grep -Fx "cloud disconnect --legacy-password-stdin --recovery-file $TEST_RECOVERY_FILE" "$TEST_LOG" >/dev/null
test "$(<"$PASSWORD_COUNT")" -eq 1

printf 'Desktop UI core-invocation test passed.\n'
