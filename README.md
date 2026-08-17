**NOTE** this project was created for personal use. I am unable to guarantee the quality or polish that one may expect from a properly maintained project.

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

## Updates

`lichen update` installs the latest release. The sync repo records the
newest lichen version that has synced with it, and a machine running an
older build pauses syncing and updates itself automatically, so
updating one machine brings the whole fleet along.
