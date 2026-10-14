# devcrate

A local CLI that runs dev Environments on cloud VMs, stops them when idle, and connects to them over SSH. Run `devcrate up` in a local clone to get an Environment for that repo, and `devcrate status` to see its state (`ready`, `running`, `busy`, `stopped` or `missing`).

Clouds sit behind Providers: executables named `devcrate-provider-<name>` on `PATH` or in `~/.config/devcrate/providers/` that speak JSON over stdin and stdout (see `docs/adr/0001-providers-are-external-executables.md`). A repo picks one in its `.devcrate.toml`:

```toml
provider = "digitalocean"
region = "ams3" # every other key is passed to the Provider unchanged
```

devcrate connects to VMs with its own SSH keypair, created on first use at `~/.config/devcrate/id_ed25519`.

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
