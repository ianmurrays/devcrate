# devcrate

A local CLI that runs dev Environments on cloud VMs, stops them when idle, and connects to them over SSH. Run `devcrate up` in a local clone to get an Environment for that repo, and `devcrate status` to see its state (`ready`, `running`, `busy`, `stopped` or `missing`).

Clouds sit behind Providers: executables named `devcrate-provider-<name>` on `PATH`, in `~/.config/devcrate/providers/` or next to the `devcrate` executable, that speak JSON over stdin and stdout (see `docs/adr/0001-providers-are-external-executables.md`). devcrate ships with the DigitalOcean and Vultr Providers.

## Install

devcrate is three kinds of file that must sit in the same directory (there is no package yet; see [Build](#build)):

- `devcrate`, the CLI you run.
- `devcrate-agent-linux-amd64` and `devcrate-agent-linux-arm64`, the Agent that `up` pushes to the VM (the one matching the VM's CPU).
- `devcrate-provider-digitalocean`, the DigitalOcean Provider (`providers/digitalocean/`).
- `devcrate-provider-vultr`, the Vultr Provider (`providers/vultr/`).

### DigitalOcean

`providers/digitalocean/README.md` has the details. In short:

1. Install `doctl` and `jq`, create a full-access personal access token and run `doctl auth init`. The Provider uses whatever `doctl` is logged in to.
2. Create a second token with only the `droplet: delete` scope and set it as `token` under `[providers.digitalocean]` in `~/.config/devcrate/config.toml`. The Agent uses it to destroy the droplet when the Environment is Idle (Self-stop). DigitalOcean cannot scope it to one droplet: it can delete every droplet in the team.
3. Build a base snapshot with Docker and the devcontainer CLI using the README's Packer recipe, and set its ID as `image`. A snapshot is only in the region it was built in; copy it to another with `doctl compute image-action transfer <snapshot-id> --region <region> --wait`.

### Vultr

`providers/vultr/README.md` has the details. In short:

1. Install `curl` and `jq`, create an API key and export it as `VULTR_API_KEY` (or have `vultr-cli` configured with it in `~/.vultr-cli.yaml`).
2. Set `region` under `[providers.vultr]` in `~/.config/devcrate/config.toml`, to one with NVMe block storage. There is no default.
3. Set `idle_timeout = 0` in the repo's `.devcrate.toml`: the Vultr Provider does not support Self-stop yet, so `up` fails with idle stop on. A stopped Environment's volume keeps billing until `delete`.

Docker's images and build cache live on the volume and slowly fill it: free space with `devcrate ssh <name> -- docker system prune`.

## Configuration

devcrate reads two TOML files and merges them, with the repo's values winning:

- `.devcrate.toml` at the top of the repo (optional, committed).
- `~/.config/devcrate/config.toml` (yours, never committed): per-Provider defaults and secrets.

```toml
# .devcrate.toml
provider = "digitalocean" # which Provider runs this repo's Environment (here or in your user config)
name = "my-app"           # optional: the Environment name (default: the repo name from origin)
idle_timeout = "1h"       # optional: Stop after this long Idle (default "30m"; 0 turns idle stop off)
devcontainer = false      # optional: VM-only, even if the repo has a devcontainer.json (default true)
region = "ams3"           # every other key is a Provider option
```

```toml
# ~/.config/devcrate/config.toml
provider = "digitalocean" # used when .devcrate.toml sets none (or there is no .devcrate.toml)
idle_timeout = "30m"      # default for repos that set none
devcontainer = true       # default for repos that set none

[providers.digitalocean]
token = "dop_v1_..."
region = "nyc3" # used when .devcrate.toml has no region
```

`provider`, `idle_timeout` and `devcontainer` can be set in either file; `.devcrate.toml` wins. `name` and `setup_script` are repo only. `--provider <name>` overrides both files.

The Provider gets its `[providers.<name>]` table overlaid with every other `.devcrate.toml` key, unchanged, and validates them itself.

`idle_timeout` is a duration such as `"30m"` or `"2h"`. Idle stop needs the Provider's Self-stop credential (DigitalOcean: `token`; Vultr: not supported yet); without it `up` fails before creating anything, unless `idle_timeout = 0`.

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

## Credentials

List the env vars and files every Environment should get in your user config:

```toml
# ~/.config/devcrate/config.toml
forward_env = ["GITHUB_TOKEN", "NPM_TOKEN"]
forward_files = ["~/.npmrc", "~/.config/gh/hosts.yml"] # files under your home directory
```

On every `up` and `devcrate ssh`, devcrate reads their current values on your laptop and writes them to the VM's tmpfs (`/run/devcrate/creds/env` and `/run/devcrate/creds/files/<path>`, mode 0600), never to the volume. So a rotated token takes effect on your next connection, and nothing is left after a Stop. Login shells on the VM and in the devcontainer source the env vars, `~/<path>` is a symlink to each file, and `/run/devcrate/creds` is mounted read-only into the devcontainer. Lifecycle hooks such as `postCreateCommand` and both setup scripts see the env vars too. An env var that is unset on your laptop, or a file that is missing, is skipped with a warning.

To keep a secret out of your shell's environment, give its value as a command instead. devcrate runs each one on your laptop with `sh -c` on every `up` and `devcrate ssh`, and forwards its stdout (trailing newline trimmed) the same way as a `forward_env` value:

```toml
# ~/.config/devcrate/config.toml
[forward_env_commands]
CLAUDE_CODE_OAUTH_TOKEN = "op read 'op://Private/Claude Code token/credential'"
```

A command that fails skips its var with a warning. A var cannot be in both `forward_env` and `[forward_env_commands]`.

`forward_env`, `forward_env_commands`, `forward_files`, `forward_agent` and `ssh_agent` are rejected in `.devcrate.toml`: a repo cannot ask for your secrets.

### Build secrets

A Dockerfile-based devcontainer can receive a `forward_env` value during its build by opting in:

```jsonc
// .devcontainer/devcontainer.json
{
  "build": {
    "dockerfile": "Dockerfile",
    "options": ["--secret", "id=NPM_TOKEN"]
  }
}
```

```dockerfile
RUN --mount=type=secret,id=NPM_TOKEN NPM_TOKEN=$(cat /run/secrets/NPM_TOKEN) npm ci
```

Limits:

- Only Dockerfile builds receive secrets. Features and image-based configs can't.
- `forward_files` are not exposed to builds.
- Never pass a secret as a build arg (`build.args`) or a Feature option: those are baked into the image, which lives on the volume.
- Secrets are not part of the Rebuild hash; run `devcrate up --rebuild` to rebuild with a rotated secret.

### Trust model

A repo you `up` is trusted with root on its VM (`docs/adr/0005-repos-are-trusted-with-their-vm.md`). Its `devcontainer up` sees every forwarded env var and your forwarded SSH agent, and its `build.options` may name any env var or root-readable file on the VM as a secret. The boundary devcrate holds is on your laptop: which secrets leave it is set only in your user config. Don't `up` a repo you don't trust while forwarding secrets.

## Connecting

`devcrate ssh [name]` opens a root shell on the VM, and `devcrate ssh [name] -- <cmd>` runs one command and exits with its exit code. It never starts a stopped Environment. `devcrate ssh --container [name]` does the same inside the running devcontainer, as its `remoteUser`, and fails rather than fall back to the VM when there is none.

The devcontainer CLI runs as root on the VM, so by default it changes a non-root `remoteUser`'s uid to 0 (it matches the uid of the user running it). To keep the `remoteUser`'s own uid, set `"updateRemoteUserUID": false` in `devcontainer.json`.

Every `up` also writes a `Host devcrate-<name>` block to `~/.ssh/devcrate_config` (the VM's address changes on every resume), and the first `up` adds `Include ~/.ssh/devcrate_config` to the top of `~/.ssh/config`. So plain `ssh devcrate-<name>` works too, with agent forwarding on. The VM's host key is kept on its volume and pinned in `~/.config/devcrate/known_hosts`, so a resume never causes a host key warning. `delete` removes the block; `stop` leaves it.

### SSH-agent forwarding

`git` over SSH works on the VM and in the devcontainer with your own keys, which never leave your laptop: devcrate forwards your SSH agent on `up` (so setup scripts, `devcontainer up` and its lifecycle hooks have it), on `devcrate ssh` and in the SSH config block. Inside the devcontainer, `SSH_AUTH_SOCK=/run/devcrate/ssh-agent/agent.sock` reaches the agent of whoever connected last and is still connected, so it keeps working across reconnects; with nobody connected it refuses connections. To give an image build your agent, opt in with `"build": {"options": ["--ssh", "default"]}` in `devcontainer.json` and `RUN --mount=type=ssh ...` in the Dockerfile.

The accepted risk: while you are connected, root on the VM (and so the repo you `up`, see ADR 0005) can use your keys, though it cannot copy them. Add keys with `ssh-add -c` to confirm every use, or load a dedicated key for git hosting only. Set `forward_agent = false` in `~/.config/devcrate/config.toml` (user config only; `.devcrate.toml` may not set it) to turn forwarding off everywhere.

devcrate forwards the SSH agent `$SSH_AUTH_SOCK` names. If your keys live in another SSH agent, such as 1Password's, set `ssh_agent` in `~/.config/devcrate/config.toml` (user config only) to forward that socket instead, on `up`, `devcrate ssh` and in the SSH config block (as `IdentityAgent`). A leading `~` expands to your home directory. `forward_agent = false` still turns forwarding off.

```toml
ssh_agent = "~/Library/Group Containers/2BUA8C4S2C.com.1password/t/agent.sock"
```

### VS Code

1. Install the Remote - SSH and Dev Containers extensions.
2. Run **Remote-SSH: Connect to Host…** and pick `devcrate-<name>`.
3. In that window, run **Dev Containers: Attach to Running Container…** and pick the Environment's devcontainer.

## Rebuilding the devcontainer

`up` keeps the existing devcontainer, so a resume is a `docker start` and `postCreateCommand` runs only once. It recreates the container (a Rebuild) when the devcontainer config on the VM changes. Edit it in the Environment's clone (`/var/lib/devcrate/workspace`): setup never pulls, so a change pushed from your laptop needs a `git pull` there first.

devcrate hashes these inputs and stores the hash at `/var/lib/devcrate/devcontainer.hash`; when it changes, `up` passes `--remove-existing-container` to `devcontainer up`:

- the `devcontainer.json` fields used to build the image or create the container: `image`, `build`, `dockerFile`, `context`, `features` (the Feature references and their options), `overrideFeatureInstallOrder`, `dockerComposeFile`, `service`, `runServices`, `workspaceMount`, `workspaceFolder`, `mounts`, `runArgs`, `appPort`, `containerEnv`, `containerUser`, `remoteUser`, `updateRemoteUserUID`, `init`, `privileged`, `capAdd` and `securityOpt`;
- the Dockerfile;
- the VM's CPU architecture.

The hash does not cover other files in the build context (files a Dockerfile `COPY`s, a Compose file's contents), the digest the `FROM` tag points to, secret values, or any other `devcontainer.json` field: editing `forwardPorts`, `customizations`, `remoteEnv` or a lifecycle command such as `postCreateCommand` does not rebuild. For those, run `devcrate up --rebuild`, which recreates the container even with no config change.

A Rebuild builds on the cached image layers, so only the changed steps rerun. It discards the old container: `postCreateCommand` runs again and the container's home starts fresh. The clone is on the VM and survives.

## Forwarding ports

```sh
devcrate forward [--no-detect] [name] [-p 8080] [-p 3001:3000]
```

Forwards `forwardPorts` from the devcontainer.json in the Environment's clone, plus each `-p <remote>` or `-p <local>:<remote>`, to `127.0.0.1` on the laptop until Ctrl-C. Ports are dialed from inside the devcontainer (on the VM when there is none), so servers bound to `127.0.0.1` there and `"host:port"` entries such as compose service names work. `portsAttributes.label` names each port in the output. A busy local port is skipped with a warning; remap it with `-p`. It also detects listening ports, including ones that start while it runs, and forwards them to the same local port (when bound to `127.0.0.1`, `::1`, `0.0.0.0` or `::`, from 1024 up to the ephemeral range), prints `<port> closed` when a detected one stops, and picks them up again after `up --rebuild`. `portsAttributes` with `"onAutoForward": "ignore"` on a port or a `"from-to"` range hides it, `otherPortsAttributes: {"onAutoForward": "ignore"}` forwards only listed ports, and `--no-detect` turns detection off for one run. `forward` never starts an Environment, exits when its SSH connection drops, and keeps the Environment from being Idle while it runs.

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
mkdir -p dist
go build -o dist/devcrate ./cmd/devcrate
for arch in amd64 arm64; do
	CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -o dist/devcrate-agent-linux-$arch ./cmd/devcrate-agent
done
cp providers/digitalocean/devcrate-provider-digitalocean providers/vultr/devcrate-provider-vultr dist/
```

Then put `dist/` on your `PATH` (or symlink `dist/devcrate` into it: devcrate resolves symlinks to find the files next to it).

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
