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

## Credentials

List the env vars and files every Environment should get in your user config:

```toml
# ~/.config/devcrate/config.toml
forward_env = ["GITHUB_TOKEN", "NPM_TOKEN"]
forward_files = ["~/.npmrc", "~/.config/gh/hosts.yml"] # files under your home directory
```

On every `up` and `devcrate ssh`, devcrate reads their current values on your laptop and writes them to the VM's tmpfs (`/run/devcrate/creds/env` and `/run/devcrate/creds/files/<path>`, mode 0600), never to the volume. So a rotated token takes effect on your next connection, and nothing is left after a Stop. Login shells on the VM and in the devcontainer source the env vars, `~/<path>` is a symlink to each file, and `/run/devcrate/creds` is mounted read-only into the devcontainer. Lifecycle hooks such as `postCreateCommand` see the env vars too. An env var that is unset on your laptop, or a file that is missing, is skipped with a warning.

These keys are rejected in `.devcrate.toml` (as is `forward_agent`): a repo cannot ask for your secrets.

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

`devcrate ssh [name]` opens a root shell on the VM, and `devcrate ssh [name] -- <cmd>` runs one command and exits with its exit code. It never starts a stopped Environment.

Every `up` also writes a `Host devcrate-<name>` block to `~/.ssh/devcrate_config` (the VM's address changes on every resume), and the first `up` adds `Include ~/.ssh/devcrate_config` to the top of `~/.ssh/config`. So plain `ssh devcrate-<name>` works too, with agent forwarding on. The VM's host key is kept on its volume and pinned in `~/.config/devcrate/known_hosts`, so a resume never causes a host key warning. `delete` removes the block; `stop` leaves it.

### VS Code

1. Install the Remote - SSH and Dev Containers extensions.
2. Run **Remote-SSH: Connect to Host…** and pick `devcrate-<name>`.
3. In that window, run **Dev Containers: Attach to Running Container…** and pick the Environment's devcontainer.

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
