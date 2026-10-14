# devcrate

A local CLI that runs dev Environments on cloud VMs, stops them when idle, and connects to them over SSH. Run `devcrate up` in a local clone to get an Environment for that repo, and `devcrate status` to see its state (`ready`, `running`, `busy`, `stopped` or `missing`).

Clouds sit behind Providers: executables named `devcrate-provider-<name>` on `PATH` or in `~/.config/devcrate/providers/` that speak JSON over stdin and stdout (see `docs/adr/0001-providers-are-external-executables.md`). A repo picks one in its `.devcrate.toml`:

```toml
provider = "digitalocean"
region = "ams3" # every other key is passed to the Provider unchanged
```

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
