**NOTE** this project was created for personal use. I am unable to guarantee the quality or polish that one may expect from a properly maintained project.

# lichen

A project for easily keeping macOS dev environments in sync.

It syncs files. Dotfiles, agent skills, slash commands, `CLAUDE.md`, app
configs: anything under your home directory. Skills and slash commands
are just files and directories, so they sync like everything else.

chezmoi does the file layer (source state, templates, per-machine data).
lichen owns when it runs: a daemon per machine watches your synced files
and pushes edits within seconds, and reacts to another machine's push by
pulling and applying. An hourly pass catches anything the events missed.

Local edits always win. A file that already exists on a machine before
lichen manages it is moved to `~/lichen-backups/` rather than
overwritten, and nothing there is ever deleted automatically.

## Setup

Create an empty private repo once (the sync repo, e.g. `you/lichen-sync`),
then on every machine:

```sh
curl -fsSL https://raw.githubusercontent.com/sylophi/lichen/HEAD/install.sh | sh
```

The installer asks for the sync repo's git URL (https or ssh, or set
`LICHEN_REPO=...` to skip the prompt). The first machine seeds the repo,
every later machine joins it: whatever is already synced gets applied,
with pre-existing local files backed up first. Then start syncing things:

```sh
lichen sync ~/.zshrc ~/.claude/CLAUDE.md ~/.claude/skills
```

## CLI

```
lichen sync <path...>     start syncing files
lichen sync               pull and apply everything now
lichen remove <path...>   stop syncing (local copies stay)
lichen list               show every synced file

lichen status [--secrets] | logs
lichen start | stop | restart | update | uninstall

aliases: rm = remove, ls = list
```

## Events

Machines talk over one ntfy topic, generated at install and carried in
the synced config. lichen publishes to it after every push it makes, so
the other machines apply in seconds without any setup.

For pushes lichen did not make (editing the repo on GitHub, a machine
without lichen), add a push webhook on the sync repo pointing at the
same topic. `lichen status --secrets` prints the URL. This is optional:
without it those pushes land on the next hourly pass.

The topic is the channel's only secret, so keep it out of pastes. An
event carries no content and only ever triggers a git pull, so the worst
a leak allows is making your machines sync early.

## Releases and updates

Pushing a `v*` tag builds darwin binaries, signs and notarizes them
(Developer ID via repo secrets), and publishes a GitHub release.
`install.sh` installs the latest release, and `lichen update`
self-updates any machine to the latest release. To hack on lichen
itself, `sh dev.sh` in a checkout builds and installs a `dev` build and
restarts the daemon.
