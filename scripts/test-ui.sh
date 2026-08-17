#!/usr/bin/env bash
set -euo pipefail

trap 'printf "Desktop UI test failed at line %s.\n" "$LINENO" >&2' ERR

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
if [[ "$*" == "snapshot inspect --details $SNAPSHOT_FILE" ]]; then
  if [[ "${UI_SCENARIO:-}" == snapshot_blocked ]]; then
    printf 'Synthetic checksum mismatch.\n' >&2
    exit 1
  fi
  warning_count=0
  if [[ "${UI_SCENARIO:-}" == snapshot_notices ]]; then
    warning_count=5
  elif [[ "${UI_SCENARIO:-}" == snapshot_informational ]]; then
    warning_count=3
  fi
  printf 'Created: 2026-08-15T01:02:03Z\nSize: 42 bytes\nFiles: 4 (42 bytes)\nPlugins: 1\nCSS themes/profiles: 2\nArtwork: 3\nWarnings: %s\n' "$warning_count"
  if [[ "$warning_count" == 5 ]]; then
    printf 'Notice: unsupported_grid_file\tsteam\tA non-image Steam grid file was not captured: steam/artwork/userdata/1/grid/100.json\nNotice: unsupported_grid_file\tsteam\tA second non-image Steam grid file was not captured: steam/artwork/userdata/1/grid/200.json\nNotice: plugin_source_unresolved\tdecky\tA plugin source needs verification.\nNotice: orphan_plugin_state\tdecky\tUnmatched Decky data state was not captured: FormerPlugin.\nNotice: orphan_plugin_state\tdecky\tUnmatched Decky settings state was not captured: FormerPlugin.\n'
  elif [[ "$warning_count" == 3 ]]; then
    printf 'Notice: plugin_source_unresolved\tdecky\tA plugin source needs verification.\nNotice: orphan_plugin_state\tdecky\tUnmatched Decky data state was not captured: FormerPlugin.\nNotice: orphan_plugin_state\tdecky\tUnmatched Decky settings state was not captured: FormerPlugin.\n'
  fi
  exit 0
fi
if [[ "$*" == "cloud list"* ]]; then
  if [[ "${UI_SCENARIO:-}" == "dashboard" ]]; then
    printf 'deck-snapshot-20260815T010203Z-dsnap-dashboard.tar.gz  42 bytes  2026-08-15T01:02:03Z\n'
  fi
  exit 0
fi
if [[ "$*" == "cloud upload"* ]]; then
  printf 'Protected snapshot uploaded and roundtrip-verified.\n'
  exit 0
fi
if [[ "$*" == "update check" ]]; then
  case "${UI_SCENARIO:-}" in
    update_available)
      printf 'Installed: v0.1.3\nAvailable: v0.1.4\nUp to date: false\n'
      ;;
    update_offline)
      printf 'Synthetic offline update check failure.\n' >&2
      exit 1
      ;;
    update_downgrade)
      printf 'Installed: v0.1.6\nAvailable: v0.1.5\nUp to date: false\n'
      ;;
    update_prerelease)
      printf 'Installed: v0.1.6\nAvailable: v0.1.7-rc.1\nUp to date: false\n'
      ;;
    *)
      printf 'Installed: v0.1.3\nAvailable: v0.1.3\nUp to date: true\n'
      ;;
  esac
  exit 0
fi
if [[ "$*" == "update install" && "${UI_SCENARIO:-}" == update_available ]]; then
  printf 'Installed: v0.1.3\nAvailable: v0.1.4\nUpdate installed: true\n'
  exit 0
fi
if [[ "${UI_SCENARIO:-}" =~ ^(restore|snapshot_restore|snapshot_noop)$ && "$*" == "restore plan $SNAPSHOT_FILE" ]]; then
  printf 'Restore plan created without target writes: %s\nPlan ID: restore-aaaaaaaaaaaaaaaa\nApproval hash: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\nActions: 1\nPlugins: 0\nBlocking: false\nRequired free space: 1 bytes\n' "$TEST_PLAN_FILE"
  exit 0
fi
if [[ "${UI_SCENARIO:-}" =~ ^(restore|snapshot_restore|snapshot_noop)$ && "$*" == "restore inspect --details $TEST_PLAN_FILE" ]]; then
  if [[ "${UI_SCENARIO:-}" == snapshot_noop ]]; then
    printf 'Plan ID: restore-aaaaaaaaaaaaaaaa\nFile action: unchanged | steam | fixture -> synthetic-target\n'
  else
    printf 'Plan ID: restore-aaaaaaaaaaaaaaaa\nPlugin action: remove | Alarm Me -> synthetic-plugin\nFile action: create | css-loader | css-loader/themes/BPM/theme.json -> synthetic-css\nFile action: replace | steam | steam/artwork/userdata/1/grid/100.png -> synthetic-artwork\n'
  fi
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
      2) printf 'tools\n' ;;
      3) printf 'doctor\n' ;;
      *) printf 'quit\n' ;;
    esac
  elif [[ "$UI_SCENARIO" == cloud ]]; then
    case "$count" in
      1) printf 'tools\n' ;;
      2) printf 'cloud\n' ;;
      3) printf 'advanced\n' ;;
      4) printf 'back\n' ;;
      *) printf 'quit\n' ;;
    esac
  elif [[ "$UI_SCENARIO" == reconnect ]]; then
    case "$count" in
      1) printf 'tools\n' ;;
      2) printf 'cloud\n' ;;
      3) printf 'connect\n' ;;
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
      4) printf 'restore\n' ;;
      *) printf 'quit\n' ;;
    esac
  elif [[ "$UI_SCENARIO" == snapshot_noop ]]; then
    case "$count" in
      1) printf 'snapshots\n' ;;
      2) printf 'local:%s\n' "${SNAPSHOT_FILE##*/}" ;;
      3) printf 'restore\n' ;;
      *) printf 'quit\n' ;;
    esac
  elif [[ "$UI_SCENARIO" == snapshot_notices || "$UI_SCENARIO" == snapshot_informational ]]; then
    case "$count" in
      1) printf 'snapshots\n' ;;
      2) printf 'local:%s\n' "${SNAPSHOT_FILE##*/}" ;;
      3) printf 'notices\n' ;;
      4) printf 'close\n' ;;
      *) printf 'quit\n' ;;
    esac
  elif [[ "$UI_SCENARIO" == snapshot_blocked ]]; then
    case "$count" in
      1) printf 'snapshots\n' ;;
      2) printf 'local:%s\n' "${SNAPSHOT_FILE##*/}" ;;
      *) printf 'quit\n' ;;
    esac
  elif [[ "$UI_SCENARIO" == decky_missing ]]; then
    case "$count" in
      1) printf 'snapshots\n' ;;
      2) printf 'local:%s\n' "${SNAPSHOT_FILE##*/}" ;;
      3) printf 'restore\n' ;;
      4) printf 'get_decky\n' ;;
      5) printf 'check_again\n' ;;
      6) printf 'close\n' ;;
      *) printf 'quit\n' ;;
    esac
  elif [[ "$UI_SCENARIO" == settings ]]; then
    case "$count" in
      1) printf 'tools\n' ;;
      2) printf 'backup\n' ;;
      *) printf 'quit\n' ;;
    esac
  elif [[ "$UI_SCENARIO" == locked_disconnect ]]; then
    case "$count" in
      1) printf 'tools\n' ;;
      2) printf 'cloud\n' ;;
      3) printf 'advanced\n' ;;
      4) printf 'disconnect\n' ;;
      *) printf 'quit\n' ;;
    esac
  elif [[ "$UI_SCENARIO" == local_upload ]]; then
    case "$count" in
      1) printf 'snapshots\n' ;;
      2) printf 'local:%s\n' "${SNAPSHOT_FILE##*/}" ;;
      3) printf 'upload\n' ;;
      4) printf 'close\n' ;;
      *) printf 'quit\n' ;;
    esac
  elif [[ "$UI_SCENARIO" == legacy_menu ]]; then
    case "$count" in
      1) printf 'tools\n' ;;
      2) printf 'cloud\n' ;;
      3) printf 'advanced\n' ;;
      4) printf 'back\n' ;;
      *) printf 'quit\n' ;;
    esac
  elif [[ "$UI_SCENARIO" == update_available ]]; then
    case "$count" in
      1) printf 'update\n' ;;
      2) printf 'update\n' ;;
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
export TZ=UTC

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
grep -F 'Export recovery key' "$KDIALOG_LOG" >/dev/null
grep -F 'Import recovery key' "$KDIALOG_LOG" >/dev/null
grep -F 'Disconnect Google Drive' "$KDIALOG_LOG" >/dev/null
if grep -F -e 'List protected cloud backups' -e 'Download protected snapshot' -e 'Upload an existing local backup' -e 'Unlock a v0.1.0 cloud connection' "$KDIALOG_LOG" >/dev/null; then
  printf 'The normal Advanced Google Drive menu exposed a legacy or technical action.\n' >&2
  exit 1
fi
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
grep -F ' Snapshots ' "$KDIALOG_LOG" >/dev/null
grep -F ' Settings & tools ' "$KDIALOG_LOG" >/dev/null
if grep -F -e ' restore Restore' -e ' settings Settings' -e ' More options ' "$KDIALOG_LOG" >/dev/null; then
  printf 'The primary dashboard retained a redundant menu action.\n' >&2
  exit 1
fi
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
test "$(grep -Fc 'update check' "$TEST_LOG")" -eq 1
if grep -F ' update Update to ' "$KDIALOG_LOG" >/dev/null; then
  printf 'The primary dashboard exposed an update action without a newer stable release.\n' >&2
  exit 1
fi

: >"$TEST_LOG"
: >"$KDIALOG_LOG"
rm -f -- "$MENU_COUNT"
export UI_SCENARIO=update_available
"$APP_DIR/deck-snapshot-ui"
grep -F ' update Update to v0.1.4 ' "$KDIALOG_LOG" >/dev/null
grep -F 'Installed: v0.1.3' "$KDIALOG_LOG" >/dev/null
grep -F 'Available: v0.1.4' "$KDIALOG_LOG" >/dev/null
grep -F 'Update now' "$KDIALOG_LOG" >/dev/null
grep -F 'Not now' "$KDIALOG_LOG" >/dev/null
grep -Fx 'update install' "$TEST_LOG" >/dev/null
test "$(grep -Fc 'update check' "$TEST_LOG")" -eq 1

: >"$TEST_LOG"
: >"$KDIALOG_LOG"
rm -f -- "$MENU_COUNT"
export UI_SCENARIO=update_offline
"$APP_DIR/deck-snapshot-ui"
grep -F 'Create Backup' "$KDIALOG_LOG" >/dev/null
grep -F ' Settings & tools ' "$KDIALOG_LOG" >/dev/null
if grep -F ' update Update to ' "$KDIALOG_LOG" >/dev/null; then
  printf 'The primary dashboard exposed an update action after an offline check failure.\n' >&2
  exit 1
fi
test "$(grep -Fc 'update check' "$TEST_LOG")" -eq 1

: >"$TEST_LOG"
: >"$KDIALOG_LOG"
rm -f -- "$MENU_COUNT"
export UI_SCENARIO=update_downgrade
"$APP_DIR/deck-snapshot-ui"
if grep -F ' update Update to ' "$KDIALOG_LOG" >/dev/null; then
  printf 'The primary dashboard exposed a downgrade as an update.\n' >&2
  exit 1
fi

: >"$TEST_LOG"
: >"$KDIALOG_LOG"
rm -f -- "$MENU_COUNT"
export UI_SCENARIO=update_prerelease
"$APP_DIR/deck-snapshot-ui"
if grep -F ' update Update to ' "$KDIALOG_LOG" >/dev/null; then
  printf 'The primary dashboard exposed a prerelease as an update.\n' >&2
  exit 1
fi

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
grep -Fx "snapshot inspect --details $SNAPSHOT_FILE" "$TEST_LOG" >/dev/null
if grep -F 'restore plan ' "$TEST_LOG" >/dev/null; then
  printf 'Snapshot browsing unexpectedly started restore planning.\n' >&2
  exit 1
fi
grep -E 'Backup date/time: (Today|Yesterday|15 Aug 2026) · 01:02' "$KDIALOG_LOG" >/dev/null
grep -F 'Decky plugins: 1' "$KDIALOG_LOG" >/dev/null
grep -F 'CSS Loader themes/profiles: 2' "$KDIALOG_LOG" >/dev/null
grep -F 'Steam artwork: 3' "$KDIALOG_LOG" >/dev/null
grep -F 'Validation: ✓ Valid' "$KDIALOG_LOG" >/dev/null
if grep -F -e 'Notices:' -e 'Valid with ' "$KDIALOG_LOG" >/dev/null; then
  printf 'A zero-notice snapshot exposed misleading warning wording.\n' >&2
  exit 1
fi
grep -F 'Restore this backup' "$KDIALOG_LOG" >/dev/null
grep -E -- '--menu Backup date/time: (Today|Yesterday|15 Aug 2026) · 01:02' "$KDIALOG_LOG" >/dev/null

: >"$TEST_LOG"
: >"$KDIALOG_LOG"
rm -f -- "$MENU_COUNT" "$FILE_COUNT" "$PASSWORD_COUNT"
export UI_SCENARIO=snapshot_notices
"$APP_DIR/deck-snapshot-ui"
grep -F 'Validation: ✓ Valid' "$KDIALOG_LOG" >/dev/null
grep -F 'Restore coverage: Partial' "$KDIALOG_LOG" >/dev/null
grep -F 'Notices: 5' "$KDIALOG_LOG" >/dev/null
grep -F 'View details' "$KDIALOG_LOG" >/dev/null
grep -F '• 2 Steam artwork logo-position metadata files were excluded' "$KDIALOG_LOG" >/dev/null
grep -F '• 1 Decky plugins need official source verification before restore' "$KDIALOG_LOG" >/dev/null
grep -F '• 1 stale or unmatched Decky plugin-state folders were excluded' "$KDIALOG_LOG" >/dev/null
if grep -F 'Valid with 5 warnings' "$KDIALOG_LOG" >/dev/null; then
  printf 'The notice summary retained the misleading validation wording.\n' >&2
  exit 1
fi

: >"$TEST_LOG"
: >"$KDIALOG_LOG"
rm -f -- "$MENU_COUNT" "$FILE_COUNT" "$PASSWORD_COUNT"
export UI_SCENARIO=snapshot_informational
"$APP_DIR/deck-snapshot-ui"
grep -F 'Notices: 3' "$KDIALOG_LOG" >/dev/null
if grep -F 'Restore coverage:' "$KDIALOG_LOG" >/dev/null; then
  printf 'Informational notices incorrectly implied partial restore coverage.\n' >&2
  exit 1
fi

: >"$TEST_LOG"
: >"$KDIALOG_LOG"
rm -f -- "$MENU_COUNT" "$FILE_COUNT" "$PASSWORD_COUNT"
export UI_SCENARIO=snapshot_blocked
"$APP_DIR/deck-snapshot-ui"
grep -F 'Snapshot blocked' "$KDIALOG_LOG" >/dev/null
grep -F 'This backup could not be validated safely.' "$KDIALOG_LOG" >/dev/null

: >"$TEST_LOG"
: >"$KDIALOG_LOG"
rm -f -- "$MENU_COUNT" "$FILE_COUNT" "$PASSWORD_COUNT"
touch "$TEST_PLAN_FILE"
export UI_SCENARIO=snapshot_restore
"$APP_DIR/deck-snapshot-ui"
test "$(grep -Fc "snapshot inspect --details $SNAPSHOT_FILE" "$TEST_LOG")" -eq 1
grep -Fx "restore plan $SNAPSHOT_FILE" "$TEST_LOG" >/dev/null
grep -Fx "restore inspect --details $TEST_PLAN_FILE" "$TEST_LOG" >/dev/null
grep -Fx "restore run --approve restore-aaaaaaaaaaaaaaaa --approval-hash aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa $TEST_PLAN_FILE" "$TEST_LOG" >/dev/null
grep -F 'Deck Snapshot will return your supported customization to this backup.' "$KDIALOG_LOG" >/dev/null
grep -F 'Remove 1 plugins added after this backup' "$KDIALOG_LOG" >/dev/null
grep -F 'Restore 1 CSS Loader themes and settings' "$KDIALOG_LOG" >/dev/null
grep -F 'A safety backup will be created first.' "$KDIALOG_LOG" >/dev/null
if grep -F -e 'Approval hash:' -e 'Plan ID:' -e "$TEST_PLAN_FILE" "$KDIALOG_LOG" >/dev/null; then
  printf 'Normal restore UI exposed technical plan identifiers or paths.\n' >&2
  exit 1
fi
if grep -F 'Deck Snapshot — Restore' "$KDIALOG_LOG" >/dev/null; then
  printf 'Snapshot restore unexpectedly opened a second snapshot chooser.\n' >&2
  exit 1
fi

: >"$TEST_LOG"
: >"$KDIALOG_LOG"
rm -f -- "$MENU_COUNT" "$FILE_COUNT" "$PASSWORD_COUNT"
export UI_SCENARIO=snapshot_noop
"$APP_DIR/deck-snapshot-ui"
grep -F 'This Steam Deck already matches this backup.' "$KDIALOG_LOG" >/dev/null
if grep -F 'restore run ' "$TEST_LOG" >/dev/null; then
  printf 'No-op restore started a transaction.\n' >&2
  exit 1
fi

: >"$TEST_LOG"
rm -f -- "$MENU_COUNT" "$FILE_COUNT" "$PASSWORD_COUNT"
export UI_SCENARIO=settings
"$APP_DIR/deck-snapshot-ui"
grep -Fx 'settings set --auto-upload true' "$TEST_LOG" >/dev/null

: >"$TEST_LOG"
rm -f -- "$MENU_COUNT" "$FILE_COUNT" "$PASSWORD_COUNT" "$HOME/.config/deck-snapshot/cloud/config-password"
export UI_SCENARIO=locked_disconnect
"$APP_DIR/deck-snapshot-ui"
grep -Fx "cloud disconnect --legacy-password-stdin --recovery-file $TEST_RECOVERY_FILE" "$TEST_LOG" >/dev/null
test "$(<"$PASSWORD_COUNT")" -eq 1

: >"$TEST_LOG"
: >"$KDIALOG_LOG"
rm -f -- "$MENU_COUNT" "$FILE_COUNT" "$PASSWORD_COUNT"
touch "$HOME/.config/deck-snapshot/cloud/config-password"
export UI_SCENARIO=local_upload
"$APP_DIR/deck-snapshot-ui"
grep -Fx "cloud upload $SNAPSHOT_FILE" "$TEST_LOG" >/dev/null
grep -F 'Storage: Local + Google Drive' "$KDIALOG_LOG" >/dev/null

: >"$TEST_LOG"
: >"$KDIALOG_LOG"
rm -f -- "$MENU_COUNT"
mkdir -p -- "$HOME/.config/deck-snapshot/cloud/legacy-v0.1.0"
touch "$HOME/.config/deck-snapshot/cloud/legacy-v0.1.0/rclone.conf" "$HOME/.config/deck-snapshot/cloud/legacy-v0.1.0/config-password"
export UI_SCENARIO=legacy_menu
"$APP_DIR/deck-snapshot-ui"
grep -F 'Legacy v0.1.0 recovery' "$KDIALOG_LOG" >/dev/null

(
  export DECK_SNAPSHOT_UI_LIBRARY=1
  # shellcheck source=/dev/null
  source "$APP_DIR/deck-snapshot-ui"
  today_name="deck-snapshot-$(TZ=UTC date '+%Y%m%dT%H%M%SZ')-dsnap-today.tar.gz"
  yesterday_name="deck-snapshot-$(TZ=UTC date -d 'yesterday' '+%Y%m%dT%H%M%SZ')-dsnap-yesterday.tar.gz"
  test "$(format_snapshot_name "$today_name")" = "$(date '+Today · %H:%M')"
  test "$(format_snapshot_name "$yesterday_name")" = "$(date -d 'yesterday' '+Yesterday · %H:%M')"
  test "$(format_snapshot_name 'deck-snapshot-20200115T134046Z-dsnap-older.tar.gz')" = "$(date -d '2020-01-15 13:40 UTC' '+%d %b %Y · %H:%M')"
  test "$(format_snapshot_size 42)" = '42 bytes'
  test "$(format_snapshot_size 1024)" = '1.0 KB'
  test "$(format_snapshot_size 109964318)" = '104.9 MB'
  test "$(format_snapshot_size invalid)" = 'Unknown'
)

printf 'Desktop UI core-invocation test passed.\n'
