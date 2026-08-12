#!/bin/sh
# Thin wrapper over `lichen uninstall`, with a raw fallback if the binary is
# missing or broken. Either way it keeps the files lichen synced into your
# home directory and the sync repo on GitHub.
set -eu

BIN="${LICHEN_INSTALL_DIR:-$HOME/.local/bin}/lichen"
if [ -x "$BIN" ]; then
  RC=0
  "$BIN" uninstall "$@" || RC=$?
  # The binary ran and gave its answer (success, or the user declined the
  # confirmation): that answer is final. Only 126/127 (present but not
  # runnable, e.g. a corrupt build) falls through to the raw teardown.
  if [ "$RC" -ne 126 ] && [ "$RC" -ne 127 ]; then
    exit "$RC"
  fi
fi

# Fallback: the binary could not run, so tear down the essentials directly.
LABEL="dev.lichen"
launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
rm -f "$HOME/Library/LaunchAgents/$LABEL.plist"
rm -f "$BIN"
echo "lichen removed (fallback). Your synced files are kept." >&2
