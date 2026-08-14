#!/bin/sh
# lichen installer: installs the latest release binary, seeds machine
# config, registers the launchd agent. Idempotent: safe to re-run for
# upgrades. To hack on lichen itself, use dev.sh in a checkout instead.
set -eu

DEST="${LICHEN_INSTALL_DIR:-$HOME/.local/bin}"
LABEL="dev.lichen"
mkdir -p "$DEST"

[ "$(uname -s)" = "Darwin" ] || { echo "lichen is macOS-only (launchd)" >&2; exit 1; }
# dev.sh installs a from-source build and points here with LICHEN_DEV=1
# for the setup steps below, without replacing its binary.
if [ "${LICHEN_DEV:-}" = 1 ]; then
  [ -x "$DEST/lichen" ] || { echo "LICHEN_DEV=1 but no $DEST/lichen (run dev.sh first)" >&2; exit 1; }
  echo "Keeping existing binary at $DEST/lichen (LICHEN_DEV=1)" >&2
else
  case "$(uname -m)" in
    arm64) ARCH=arm64 ;;
    x86_64) ARCH=x64 ;;
    *) echo "Unsupported architecture: $(uname -m)" >&2; exit 1 ;;
  esac
  URL="https://github.com/dittofleet/lichen/releases/latest/download/lichen-darwin-$ARCH"
  TMP=$(mktemp)
  trap 'rm -f "$TMP"' EXIT
  echo "Downloading $URL..." >&2
  curl -fsSL "$URL" -o "$TMP"
  chmod +x "$TMP"
  mv "$TMP" "$DEST/lichen"
  echo "Installed $DEST/lichen" >&2
fi

# Stop any already-running daemon up front. On an upgrade re-run it would
# otherwise stay live through the config apply and reconcile below, racing
# this script on the same git/chezmoi state (the script drives chezmoi
# directly and never takes lichen's lock). It is re-registered at the end.
GUI_DOMAIN="gui/$(id -u)"
launchctl bootout "$GUI_DOMAIN/$LABEL" 2>/dev/null || true

command -v chezmoi >/dev/null 2>&1 || {
  echo "Installing chezmoi (lichen's file engine)..." >&2
  brew install -q chezmoi
}

# The sync repo is required: it is a chezmoi source repo whose managed
# files include lichen's own config.json, so applying it brings the whole
# synced state to this machine. LICHEN_REPO=<git url> (https or ssh)
# skips the prompt.
CONFIG_FILE="$HOME/.config/lichen/config.json"
SRC=$(chezmoi source-path 2>/dev/null || true)

DID_INIT=0
if [ -n "$SRC" ] && [ -d "$SRC/.git" ]; then
  echo "chezmoi already initialized at $SRC (left untouched)" >&2
else
  if [ -z "${LICHEN_REPO:-}" ] && [ -r /dev/tty ]; then
    printf "Sync repo (git URL): " >&2
    IFS= read -r LICHEN_REPO < /dev/tty || LICHEN_REPO=""
  fi
  if [ -z "${LICHEN_REPO:-}" ]; then
    echo "A sync repo git URL is required. Create an empty private repo and re-run" >&2
    echo "with LICHEN_REPO=<git url> (or answer the prompt)." >&2
    exit 1
  fi
  EXPANDED=$LICHEN_REPO
  case "$EXPANDED" in "~"*) EXPANDED="$HOME${EXPANDED#\~}" ;; esac
  if [ -d "$EXPANDED" ]; then
    echo "$LICHEN_REPO is a local directory. Pass the repo's git URL instead" >&2
    echo "(for example: git -C \"$LICHEN_REPO\" remote get-url origin)." >&2
    exit 1
  fi
  echo "Initializing sync repo from $LICHEN_REPO..." >&2
  chezmoi init "$LICHEN_REPO"
  DID_INIT=1
fi

# Guard against the wrong repo. A valid sync repo is either empty (a first
# machine, which seeds config.json below) or already carries lichen's own
# config.json (a joining machine). A repo that has files but NOT that config
# is not a lichen sync repo, most likely the lichen SOURCE repo passed by
# mistake (their names differ by one suffix). Refuse rather than start
# managing arbitrary files as dotfiles.
MANAGED=$(chezmoi managed --path-style=absolute 2>/dev/null || true)
HAS_CONFIG=0
printf '%s\n' "$MANAGED" | grep -qxF "$CONFIG_FILE" && HAS_CONFIG=1
if [ -n "$MANAGED" ] && [ "$HAS_CONFIG" = 0 ]; then
  echo "The repo has files but no $CONFIG_FILE, so it is not a lichen sync repo." >&2
  echo "Did you point at the lichen SOURCE repo instead of your sync repo?" >&2
  echo "The sync repo is a separate, initially empty, private repo." >&2
  # If this run just cloned it, undo that so the machine is not left wired to
  # the wrong source.
  if [ "$DID_INIT" = 1 ]; then
    BAD_SRC=$(chezmoi source-path 2>/dev/null || true)
    if [ -n "$BAD_SRC" ] && [ -d "$BAD_SRC/.git" ]; then
      rm -rf "$BAD_SRC"
      echo "Removed the incorrectly initialized source. Re-run with the right repo." >&2
    fi
  fi
  exit 1
fi

# Materialize the config first (everything else needs it). A pre-existing
# local config is backed up, the sync repo's copy wins.
if [ "$HAS_CONFIG" = 1 ]; then
  mkdir -p "$(dirname "$CONFIG_FILE")"
  if [ -f "$CONFIG_FILE" ]; then
    # Mirrors the binary's backup policy (internal/backup): move, not
    # copy, into ~/lichen-backups/<timestamp>/<home-relative-path>.
    BK="$HOME/lichen-backups/$(date +%Y-%m-%d-%H%M%S)/.config/lichen"
    mkdir -p "$BK" && mv "$CONFIG_FILE" "$BK/config.json"
    echo "Backed up existing config to $BK/config.json" >&2
  fi
  chezmoi apply --force "$CONFIG_FILE"
  echo "Synced config applied" >&2
fi

# First machine only (a joining machine received its config above): seed
# the starter config with a fresh event topic.
if [ ! -f "$CONFIG_FILE" ]; then
  mkdir -p "$HOME/.config/lichen"
  TOPIC="lichen-$(LC_ALL=C tr -dc 'a-z0-9' < /dev/urandom | head -c 12)"
  printf '{\n  "topic_prefix": "%s"\n}\n' "$TOPIC" > "$CONFIG_FILE"
  # The topic is a shared secret: keep the file out of other users' reach.
  chmod 600 "$CONFIG_FILE"
  echo "Created starter config at $CONFIG_FILE" >&2
  echo "  event topic: $TOPIC (a shared secret across your machines)" >&2
  echo "  It will sync to your other machines through the sync repo." >&2
else
  echo "Config already exists at $CONFIG_FILE (left untouched)" >&2
fi

# launchd is macOS's daemon manager: RunAtLoad starts lichen at login,
# KeepAlive restarts it if it dies. Agents get a minimal PATH, so bun,
# homebrew git etc. must be added explicitly.
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
mkdir -p "$HOME/Library/LaunchAgents" "$HOME/Library/Logs"
cat > "$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$LABEL</string>
  <key>ProgramArguments</key>
  <array>
    <string>$DEST/lichen</string>
    <string>daemon</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>30</integer>
  <key>StandardOutPath</key><string>$HOME/Library/Logs/lichen.log</string>
  <key>StandardErrorPath</key><string>$HOME/Library/Logs/lichen.log</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key><string>$HOME/.bun/bin:$HOME/.local/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin</string>
  </dict>
</dict>
</plist>
EOF

# First full pass in the terminal: synced files applied, with pre-existing
# local files backed up to ~/lichen-backups, all before the daemon takes
# over. The old daemon was already booted out above, so this runs unopposed.
"$DEST/lichen" sync || true

launchctl bootstrap "$GUI_DOMAIN" "$PLIST"
echo "Daemon registered with launchd as $LABEL" >&2
echo "Start syncing with 'lichen sync <path...>'." >&2
