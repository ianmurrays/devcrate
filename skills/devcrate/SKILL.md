---
name: devcrate
description: "Set up and use a devcrate Environment (a cloud dev VM) for the current repo: write `.devcrate.toml`, run `devcrate up`, check its state, run commands on it over SSH. Use when the user mentions devcrate, asks for a cloud dev environment for this repo, or the repo has a `.devcrate.toml`."
---

# devcrate

devcrate runs an **Environment** for a repo: a cloud VM with a persistent disk, optionally running the repo's devcontainer. It **Stops** the VM when Idle (disk kept) and connects over SSH. The full manual is the [README](https://github.com/ianmurrays/devcrate/blob/main/README.md); the links below go to its sections. When the README and an error from `devcrate` disagree, the error is right: the installed binary may be older or newer than `main`.

## Steps

1. **Check devcrate works.** Run `devcrate list --json`. If `devcrate` is not found or the command fails, stop: tell the human to follow [Install](https://github.com/ianmurrays/devcrate#install), which needs cloud tokens and a base image only they can create. Done when `list --json` prints JSON.
2. **Pick the Provider.** `provider` comes from `.devcrate.toml`, else from `~/.config/devcrate/config.toml`. If neither sets it, ask the human which Provider to use, and whether to commit it in `.devcrate.toml` (every teammate gets it) or have them set it in their user config. Done when a Provider is named in one of the two files.
3. **Write the repo config.** Create or edit `.devcrate.toml` at the repo root, and `.devcontainer/devcontainer.json` if the repo should run in a container. Keys are in [Configuration](https://github.com/ianmurrays/devcrate#configuration); installing tools is in [Setup scripts](https://github.com/ianmurrays/devcrate#setup-scripts). Done when every key you set is one the README names, or a Provider option from its README.
4. **Push what the VM reads.** `up` reads `.devcrate.toml` on the laptop, but the VM clones `origin` and never pulls: `devcontainer.json` and the `setup_script` reach it only through git. Commit and push them, with the human's go-ahead. The first `up` clones `origin`'s default branch. To get later changes, or changes on another branch, into an existing Environment, run `devcrate ssh <name> -- git pull` (or `git checkout <branch>`), then `up` again. Done when the VM's clone has them.
5. **Start it.** Run `devcrate up`. It creates or resumes the VM and runs setup, which can take several minutes. On a non-zero exit, read `up`'s own error first: it fails before setup on a name clash, a missing Self-stop credential or a Provider error. If the error is from setup, read `devcrate ssh <name> -- cat /var/lib/devcrate/setup.log`. Fix the cause and run `up` again.
6. **Verify.** Run `devcrate status --json`. Done when it prints `"state":"ready"`.
7. **Use it.** `devcrate ssh <name> -- <cmd>` runs one command in the VM's clone and exits with its code; add `--container` to run in the devcontainer. `devcrate forward [name] -p <port>` forwards ports until Ctrl-C or until its SSH connection drops ([Forwarding ports](https://github.com/ianmurrays/devcrate#forwarding-ports)).

## What you may run

- Freely: `up`, `status`, `list`, `ssh`, `forward`, `keep-alive`.
- After the human confirms: `stop`, and `up --rebuild`, which starts the container's home fresh.
- Only when the human explicitly asks for it: `delete --yes`. Delete removes the persistent disk and every uncommitted change on it.

## The user config is the human's

`~/.config/devcrate/config.toml` holds Provider tokens and decides which secrets leave the laptop (`forward_env`, `forward_env_commands`, `forward_files`, `forward_repo_files`, `forward_agent`, `ssh_agent`, `git_signing_key`). Edit only repo files. When a task needs a user-config change, show the human the exact TOML to add and let them apply it. `.devcrate.toml` is committed, so secrets, Provider tokens included, belong only in the user config. See [Credentials](https://github.com/ianmurrays/devcrate#credentials).

## Gotchas

- **Names**: `name` uses only `a-z`, `0-9` and `-`, at most 32 characters. It defaults to the repo name from `origin`; if `up` reports a clash with another repo, set `name` in `.devcrate.toml`.
- **Setup scripts run on every `up`**, as root, so make them idempotent: check before installing, never append to a file twice.
- **The VM's clone** is `/var/lib/devcrate/workspace`. A quick devcontainer fix can be made there directly, then pushed from there, so the laptop and the VM do not diverge.
- **Rebuild**: `up` recreates the devcontainer only when its build inputs change. After editing a lifecycle command such as `postCreateCommand`, run `devcrate up --rebuild` ([Rebuilding](https://github.com/ianmurrays/devcrate#rebuilding-the-devcontainer)).
- **Gitignored secrets in the working tree**, such as Rails' `config/credentials/*.key`, are missing from the VM's clone. Ask the human to list them under `[forward_repo_files]` ([Files in the clone](https://github.com/ianmurrays/devcrate#files-in-the-clone)).
- **Forwarded env vars are set in login shells only.** Wrap a one-off command that needs them: `devcrate ssh <name> -- 'bash -lc "gh auth status"'`.
- **Vultr** has no Self-stop yet: set `idle_timeout = 0` in `.devcrate.toml`, or `up` fails.
- **Long work with nobody connected** gets Stopped once Idle. Hold it first with `devcrate keep-alive <name> 2h` ([Keep-alive](https://github.com/ianmurrays/devcrate#keep-alive)).
