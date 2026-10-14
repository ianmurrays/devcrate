# devcrate

A local CLI that runs dev Environments on cloud VMs, stops them when idle, and connects to them over SSH. Run `devcrate up` in a local clone to get an Environment for that repo, and `devcrate status` to see its state (`ready`, `running`, `busy`, `stopped` or `missing`).

Clouds sit behind Providers: executables named `devcrate-provider-<name>` on `PATH` or in `~/.config/devcrate/providers/` that speak JSON over stdin and stdout (see `docs/adr/0001-providers-are-external-executables.md`).

## Configuration

devcrate reads two TOML files and merges them, with the repo's values winning:

- `.devcrate.toml` at the top of the repo (optional, committed).
- `~/.config/devcrate/config.toml` (yours, never committed): per-Provider defaults and secrets.

```toml
# .devcrate.toml
provider = "digitalocean" # required: which Provider runs this repo's Environment
name = "my-app"           # optional: the Environment name (default: the repo name from origin)
region = "ams3"           # every other key is a Provider option
```

```toml
# ~/.config/devcrate/config.toml
[providers.digitalocean]
token = "dop_v1_..."
region = "nyc3" # used when .devcrate.toml has no region
```

The Provider gets its `[providers.<name>]` table overlaid with every `.devcrate.toml` key except `provider` and `name`, unchanged, and validates them itself.

Environment names use only `a-z`, `0-9` and `-`, and are at most 32 characters. If two repos derive the same name, `up` aborts setup in the second one and asks you to set `name` in its `.devcrate.toml`.

`devcrate up <git-url>` works outside any clone: it reads `.devcrate.toml` from the repo's default branch with a shallow, blob-less clone that uses your local git credentials.

`stop`, `delete` and `status` take an optional `[name]`. Inside a repo it defaults to the repo's Environment; outside one it is required. A name is looked up in every Provider configured under `[providers.*]` (and the current repo's); if more than one has it, pass `--provider <name>`. Every command takes `--provider <name>` to skip the lookup, and `list` shows the Environments of every configured Provider.

devcrate connects to VMs with its own SSH keypair, created on first use at `~/.config/devcrate/id_ed25519`.

## Setup scripts

Two scripts you write can install tools on an Environment. Both run in `up`'s SSH session, as root, on every `up`, so they must be idempotent: check before installing, and don't append to a file twice.

- **Repo script:** `setup_script = "path/in/repo.sh"` in `.devcrate.toml` (repo only). It runs on the VM in the clone at `/var/lib/devcrate/workspace`, after the clone and before `devcontainer up`. It is run directly if it is executable, else with `sh`. `up` never pulls, so it runs the script as it is in the VM's clone.
- **User script:** `~/.config/devcrate/setup.sh`, pushed over SSH on every `up` and run with `sh` on the VM after the repo script and `devcontainer up`. With `setup_script_in_container = true` in `~/.config/devcrate/config.toml` (user config only; ignored in `.devcrate.toml`) it runs inside the devcontainer instead, as the container's `remoteUser`. In a VM-only Environment it always runs on the VM.

A non-zero exit from either script fails setup: `up` exits 1 and the Environment is not Ready. Fix the script and run `up` again.

Install your dotfiles from the user script, with `setup_script_in_container = true` if you work in the devcontainer:

```sh
# ~/.config/devcrate/setup.sh
set -eu
if [ ! -d ~/dotfiles ]; then
	git clone https://github.com/you/dotfiles ~/dotfiles
fi
git -C ~/dotfiles pull -q
~/dotfiles/install.sh
```

All setup output, including both scripts', is also written to `/var/lib/devcrate/setup.log` on the volume (the latest `up` only): `devcrate ssh <name> -- cat /var/lib/devcrate/setup.log`.

## Connecting

`devcrate ssh [name]` opens a root shell on the VM, and `devcrate ssh [name] -- <cmd>` runs one command and exits with its exit code. It never starts a stopped Environment.

Every `up` also writes a `Host devcrate-<name>` block to `~/.ssh/devcrate_config` (the VM's address changes on every resume), and the first `up` adds `Include ~/.ssh/devcrate_config` to the top of `~/.ssh/config`. So plain `ssh devcrate-<name>` works too, with agent forwarding on. The VM's host key is kept on its volume and pinned in `~/.config/devcrate/known_hosts`, so a resume never causes a host key warning. `delete` removes the block; `stop` leaves it.

### VS Code

1. Install the Remote - SSH and Dev Containers extensions.
2. Run **Remote-SSH: Connect to Host…** and pick `devcrate-<name>`.
3. In that window, run **Dev Containers: Attach to Running Container…** and pick the Environment's devcontainer.

## Keep-alive

An Environment stops itself once it has been Idle (no SSH connection, no setup running, no Keep-alive) for `idle_timeout`. To leave work running with nobody connected, hold it with a Keep-alive:

```sh
devcrate keep-alive [name] 2h        # from the laptop, over SSH
devcrate-agent keep-alive 2h         # on the VM, e.g. from a script
```

The duration is Go-style (`90s`, `30m`, `2h30m`). The hold ends on its own: the Environment then stops after a further `idle_timeout` with nobody connected. Each call replaces the previous hold, so a shorter one cuts it short. The hold lives in tmpfs (`/run/devcrate/keep-alive`), so a reboot or Stop drops it. `devcrate keep-alive` never starts a stopped Environment and fails like `devcrate ssh` does.

## Build

Requires Go 1.25.

```sh
go build ./cmd/devcrate
```

## Test

Unit tests:

```sh
go test ./...
```

End-to-end tests drive the real `devcrate` binary against a test-only fake Provider (`e2e/fakeprovider`) whose "VM" is a privileged systemd container built from `e2e/vm/Dockerfile`. They need a running Docker daemon (Docker Desktop or OrbStack); the first run builds the VM image, which takes a few minutes.

```sh
go test -tags e2e -timeout 20m ./e2e/
```

If a run is killed before it cleans up, remove what it left behind with the commands below (they also remove the VMs of any e2e run still in progress):

```sh
docker ps -aq --filter label=devcrate.fake=1 | xargs docker rm -f -v
docker volume ls -q --filter label=devcrate.fake=1 | xargs docker volume rm
```
