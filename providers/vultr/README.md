# Vultr Provider

`devcrate-provider-vultr` ships with devcrate: it sits next to the `devcrate` executable, where devcrate looks for Providers after `PATH` and `~/.config/devcrate/providers/`. It is a shell script that speaks the Provider contract (`docs/adr/0001-providers-are-external-executables.md`) by calling the [Vultr API](https://www.vultr.com/api/) with `curl` and `jq` on your laptop. It does not use `vultr-cli`.

An Environment `<name>` is an instance named `devcrate-<name>` and tagged `devcrate:<name>`, and a NVMe (`high_perf`) block volume labelled `devcrate-<name>`. Vultr volumes have no tags, so the Provider finds them by label. The volume is mounted at `/var/lib/devcrate` and holds the repo, `/root`, Docker's images and containers, and the devcontainer.

- `up` creates the volume if it is missing, then an instance, waits for it to be active, and attaches the volume live (or starts an instance that is halted).
- `stop` destroys the instance and keeps the volume, which Vultr detaches by itself. A stopped Environment costs only the volume's storage (see [Cost](#cost)).
- `delete` destroys the instance and the volume.
- `status` reads the instance by tag and the volume by label. `list` reads every volume labelled `devcrate-*`, since a stopped Environment has nothing else.

```toml
# .devcrate.toml
provider = "vultr"
idle_timeout = 0 # required for now: see "Idle stop" below
```

```toml
# ~/.config/devcrate/config.toml
[providers.vultr]
region = "ewr"
```

## Idle stop is not supported yet

Self-stop on Vultr needs a credential scoped to the one instance, which `up` does not mint yet. Until it does, the `self-stop` verb fails, so `up` fails before creating anything unless idle stop is off:

```
vultr: Self-stop is not supported yet: set idle_timeout = 0 in .devcrate.toml, or at the top of ~/.config/devcrate/config.toml (see the Provider's README.md)
```

Set `idle_timeout = 0` in the repo's `.devcrate.toml` (a top-level `idle_timeout = 0` in your user config turns idle stop off for every Provider). An Environment then runs, and bills, until you `devcrate stop` it.

## API key and tools

Install `curl` and `jq` (macOS: `brew install jq`; `curl` ships with macOS). If either is missing, every verb fails with `vultr: curl is not installed ...` / `jq is not installed ...`.

Create an API key in the control panel (**Account → API**). If you restrict it under **Access Control** there, allow your laptop's IP. The Provider reads the key from `VULTR_API_KEY`, or else from `api-key` in `~/.vultr-cli.yaml` (where `vultr-cli` keeps it):

```sh
export VULTR_API_KEY=...
```

```yaml
# ~/.vultr-cli.yaml
api-key: ...
```

Without either, every verb fails with `vultr: no API key: set VULTR_API_KEY, or api-key in ~/.vultr-cli.yaml`. Errors from the API reach you as Vultr words them, for example `vultr: POST /blocks: Due to account inactivity, please verify your payment details`.

The first `up` imports devcrate's public key (`~/.config/devcrate/id_ed25519.pub`) into the account as an SSH key named `devcrate`. It is shared by all your Environments, so `delete` leaves it.

## Options

| Option | Default | |
| --- | --- | --- |
| `region` | none, required | Region ID, such as `ewr` or `ams`. It must offer NVMe block storage (`block_storage_high_perf` in `GET /v2/regions`); in a region that only has HDD block storage, `up` fails with `vultr: region "<id>" has no NVMe block storage (high_perf)`. The volume and every instance of an Environment live in the region its volume was created in; changing `region` later makes `up` fail. |
| `plan` | `vc2-2c-4gb` | Instance plan. 4 GB of memory is enough to build most devcontainers. Change it any time: it applies from the next `up` after a Stop. |
| `image` | the newest x64 Ubuntu LTS | Snapshot ID of a base image (see below), passed as `snapshot_id`. Without it, the instance runs the newest `Ubuntu NN.04 LTS x64` from `GET /v2/os`. |
| `volume_size` | `50` | Volume size in GB (10 to 10000), used only when the volume is created. |

There is no `token` option: the Provider uses your API key, and Self-stop will mint its own scoped credential.

## The base image

devcrate expects the instance to come up with Docker (29 or newer) and the devcontainer CLI (0.80.0 or newer, at `/opt/devcontainers`) installed. `up` installs either one if it is missing, so stock Ubuntu works, but on every resume of a fresh instance that costs a minute or more. Bake them into a snapshot instead. Do not bake in the Agent: `up` pushes the version matching your `devcrate`.

1. Create an Ubuntu LTS instance in the control panel, then install both over SSH:

   ```sh
   curl -fsSL https://get.docker.com | sh
   curl -fsSL https://raw.githubusercontent.com/devcontainers/cli/main/scripts/install.sh -o /tmp/install-devcontainers.sh && sh /tmp/install-devcontainers.sh --prefix /opt/devcontainers
   /opt/devcontainers/bin/devcontainer --version
   ```

2. Snapshot it, wait for the snapshot to be `complete`, and destroy the instance:

   ```sh
   curl -sS -X POST -H "Authorization: Bearer $VULTR_API_KEY" -H "Content-Type: application/json" \
     --data '{"instance_id": "<instance-id>", "description": "devcrate-base"}' https://api.vultr.com/v2/snapshots
   curl -sS -H "Authorization: Bearer $VULTR_API_KEY" https://api.vultr.com/v2/snapshots | jq '.snapshots[] | {id, description, status}'
   ```

3. Set the snapshot's `id` as `image`.

## Cost

- **The instance** bills by the hour at the plan's rate (see [Vultr's pricing](https://www.vultr.com/pricing/)) while it exists. `stop` destroys it, which ends that.
- **The volume** bills at $0.10/GB per month (`volume_size = 50` is $5 a month), **whether or not it is attached**. A stopped Environment keeps billing for its volume until you `devcrate delete` it.
- **A snapshot**, if you use one, bills for its storage until you delete it in the control panel.

## Freeing volume space

Docker's images, build cache and stopped containers live on the volume, so they survive a Stop and slowly fill it. Clean up over `devcrate ssh` while the Environment is Ready:

```sh
devcrate ssh <name> -- df -h /var/lib/devcrate
devcrate ssh <name> -- docker system prune      # stopped containers, dangling images, build cache
devcrate ssh <name> -- docker system prune -a   # also every image no container uses
```

To grow the volume instead, resize it in the control panel, then `devcrate ssh <name> -- resize2fs /dev/vdb`.

## Live smoke test

`providers/vultr/live_test.go` runs `up`, `stop`, a resume and `delete` against a real account, and logs how long the first `up` and the resume took. It is skipped unless `VULTR_API_KEY`, `DEVCRATE_VULTR_SMOKE_REGION` and `DEVCRATE_VULTR_SMOKE_REPO` (a public repo with a `devcontainer.json`) are set; `DEVCRATE_VULTR_SMOKE_IMAGE` and `DEVCRATE_VULTR_SMOKE_PLAN` are optional. It creates billed resources and deletes them at the end:

```sh
go test -v -run TestLiveSmoke -timeout 60m ./providers/vultr/
```
