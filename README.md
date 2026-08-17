# lichen

A project for easily keeping macOS dev environments in sync.

It syncs things, one module per kind of thing:

- **files**: dotfiles, slash commands, `CLAUDE.md`, app configs, anything
  under your home directory. Your own skills and slash commands are just
  files and directories, so they sync like everything else.
- **skills**: agent skills installed from public repos, kept up to date
  by polling.

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

## Skills

The skills module installs agent skills (`SKILL.md` directories, the
format the [Vercel skills CLI](https://github.com/vercel-labs/skills)
uses) from public repos:

```sh
lichen skills add vercel-labs/agent-skills --skill frontend-design
lichen skills add owner/single-skill-repo
```

The canonical copy lands in `~/.agents/skills/<name>` with a relative
symlink from `~/.claude/skills/<name>`, the same layout `npx skills`
creates, so Claude Code picks the skill up globally and both tools
coexist: lichen never touches a skill it didn't install. Other harnesses
get links too by listing their skill directories in the skills config:

```json
{ "harnesses": ["~/.claude/skills", "~/.codex/skills"] }
```

The list of skill repos lives in its own synced config file
(`~/.config/lichen/skills.json`), so adding a skill on one machine
installs it everywhere, and removing one uninstalls it everywhere. The
daemon polls each repo about hourly, and a repo moving only reinstalls
the skills whose content actually changed. `lichen skills update` checks
immediately.

## Updates

`lichen update` installs the latest release. The sync repo records the
newest lichen version that has synced with it, and a machine running an
older build pauses syncing and updates itself automatically, so
updating one machine brings the whole fleet along.
