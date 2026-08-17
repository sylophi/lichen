# lichen

A project for easily keeping macOS dev environments in sync.

It syncs files. Dotfiles, agent skills, slash commands, `CLAUDE.md`, app
configs: anything under your home directory. Skills and slash commands
are just files and directories, so they sync like everything else.

lichen is a pun on 'liken' and just how lichen behaves in general.

## Setup

Prereqs on each machine: [Homebrew](https://brew.sh) (installs chezmoi),
and git access that can clone the sync repo (SSH key or credentials,
since the repo should be private).

Create an empty private repo once (the sync repo, e.g. `you/lichen-sync`),
then on every machine:

```sh
curl -fsSL https://raw.githubusercontent.com/dittofleet/lichen/HEAD/install.sh | sh
```

Then start syncing things:

```sh
lichen sync ~/.zshrc ~/.claude/CLAUDE.md
```

Deletions sync too: delete a synced file on one machine and every other
machine deletes its copy (moved into `~/lichen-backups/` first, never
destroyed). A file deleted by mistake can be brought back from the sync
repo's git history, on all machines at once:

```sh
lichen recover ~/.zshrc
```
