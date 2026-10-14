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
