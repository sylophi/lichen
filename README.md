**NOTE** this project was created for personal use. I am unable to guarantee the quality or polish that one may expect from a properly maintained project.

# lichen

A project for easily keeping macOS dev environments in sync.

It syncs files. Dotfiles, agent skills, slash commands, `CLAUDE.md`, app
configs: anything under your home directory. Skills and slash commands
are just files and directories, so they sync like everything else.

lichen is a pun on 'liken' and just how lichen behaves in general.

## Setup

Create an empty private repo once (the sync repo, e.g. `you/lichen-sync`),
then on every machine:

```sh
curl -fsSL https://raw.githubusercontent.com/sylophi/lichen/HEAD/install.sh | sh
```

Then start syncing things:

```sh
lichen sync ~/.zshrc ~/.claude/CLAUDE.md
```
