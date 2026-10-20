# Vultr Provider

`devcrate-provider-vultr` ships with devcrate: it sits next to the `devcrate` executable, where devcrate looks for Providers after `PATH` and `~/.config/devcrate/providers/`. It is a shell script that speaks the Provider contract (`docs/adr/0001-providers-are-external-executables.md`) by calling the [Vultr API](https://www.vultr.com/api/) with `curl` and `jq` on your laptop. It does not use `vultr-cli`.

An Environment `<name>` is an instance named `devcrate-<name>` and tagged `devcrate:<name>`, and an NVMe (`high_perf`) block volume labelled `devcrate-<name>`. Vultr volumes have no tags, so the Provider finds them by label. The volume is mounted at `/var/lib/devcrate` and holds the repo, `/root`, Docker's images and containers, and the devcontainer.

- `up` creates the volume if it is missing, then an instance, waits for it to be active, and attaches the volume live (or starts an instance that is halted). It then mints the Self-stop key (see [Idle stop](#idle-stop)).
- `stop` destroys the instance and keeps the volume, which Vultr detaches by itself. A stopped Environment costs only the volume's storage (see [Cost](#cost)).
- `delete` destroys the instance and the volume, and the Self-stop service user, policy and key.
- `save-image` saves the repo's Base image from the instance and destroys it, keeping the volume (see [The Base image](#the-base-image)).
- `status` reads the instance by tag and the volume by label, and returns the Base image `up` boots from. `list` reads every volume labelled `devcrate-*`, since a stopped Environment has nothing else.

```toml
# .devcrate.toml
provider = "vultr"
```

```toml
# ~/.config/devcrate/config.toml
[providers.vultr]
region = "ewr"
```

## Idle stop

When the Environment is Idle, the Agent destroys its own instance with a key that can do nothing else. The volume detaches by itself and stays, as after `devcrate stop`.

On the first `up`, the Provider creates a service user and an IAM policy, both named `devcrate-<name>` (Vultr requires an email and a password for the user: the Provider makes up both, and neither is used). On every `up`, it:

1. points the policy at the new instance: `compute.instance.Delete` on `instance:<id>`, nothing else;
2. sets the user's IP allowlist to the instance's IPv4 address alone (a service user's allowlist starts empty, which rejects every call);
3. mints a key for the user that never expires, and revokes every other key the user has.

devcrate writes the key to a root-only tmpfs file on the instance (`/run/devcrate/self-stop.env`), never to the volume. The key does not expire because an expired key would leave a long Keep-alive Environment unable to Self-stop; the policy and the allowlist already limit it to deleting one instance, from that instance. `delete` removes the user (and so its key) and the policy. They show up in the control panel under **Account → Users** and **Account → Policies**.

## API key and tools

Install `curl` and `jq` (macOS: `brew install jq`; `curl` ships with macOS). If either is missing, every verb fails with `vultr: curl is not installed ...` / `jq is not installed ...`.

Create an API key in the control panel (**Account → API**). If you restrict it under **Access Control** there, allow your laptop's IP. It must be able to manage users and policies, which your own account's key can, for Self-stop. The Provider reads the key from `VULTR_API_KEY`, or else from `api-key` in `~/.vultr-cli.yaml` (where `vultr-cli` keeps it):

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
| `image` | the newest x64 Ubuntu LTS | Snapshot ID of an account-wide image (see [The Base image](#the-base-image)), passed as `snapshot_id`. The repo's Base image takes precedence over it. Without either, the instance runs the newest `Ubuntu NN.04 LTS x64` from `GET /v2/os`. |
| `volume_size` | `50` | Volume size in GB (10 to 10000), used only when the volume is created. |

There is no `token` option: the Provider uses your API key, and mints Self-stop's own key (see [Idle stop](#idle-stop)).

## The Base image

devcrate expects the instance to come up with Docker (29 or newer) and the devcontainer CLI (0.80.0 or newer, at `/opt/devcontainers`) installed. `up` installs either one if it is missing, so stock Ubuntu works, but on every resume of a fresh instance that costs a minute or more, as does whatever your setup scripts install on the VM, such as the [GitHub CLI](../../README.md#github-cli). Save the repo's [Base image](../../README.md#base-image) to skip that:

```sh
devcrate save-image
```

The Provider halts the instance, snapshots it with the description `devcrate-base:<base name>` (the repo's base Environment name, shared by its worktree Environments), and waits for the snapshot to be `complete`, printing progress, which takes several minutes. It then deletes every older snapshot with that description and destroys the instance, keeping the volume. If the snapshot fails, the old Base image stays and so does the halted instance: `devcrate up` starts it again.

From then on, `up` boots every Environment of the repo from the newest `complete` snapshot with that description, in any region. `devcrate status` shows it. `devcrate save-image --delete` deletes it, touching no Environment, and `devcrate delete` leaves it.

To share one image across every repo instead, build a snapshot yourself and set its ID as `image`: `up` uses it for repos without a Base image. Do not bake in the Agent: `up` pushes the version matching your `devcrate`.

## Cost

- **The instance** bills by the hour at the plan's rate (see [Vultr's pricing](https://www.vultr.com/pricing/)) while it exists. `stop` destroys it, which ends that.
- **The volume** bills at $0.10/GB per month (`volume_size = 50` is $5 a month), **whether or not it is attached**. A stopped Environment keeps billing for its volume until you `devcrate delete` it.
- **The Base image**, and any snapshot you set as `image`, bills for its storage until you delete it: the Base image with `devcrate save-image --delete`, other snapshots in the control panel.

## Freeing volume space

Docker's images, build cache and stopped containers live on the volume, so they survive a Stop and slowly fill it. Clean up over `devcrate ssh` while the Environment is Ready:

```sh
devcrate ssh <name> -- df -h /var/lib/devcrate
devcrate ssh <name> -- docker system prune      # stopped containers, dangling images, build cache
devcrate ssh <name> -- docker system prune -a   # also every image no container uses
```

To grow the volume instead, resize it in the control panel, then `devcrate ssh <name> -- resize2fs /dev/vdb`.

## Live smoke test

`providers/vultr/live_test.go` runs `up`, `stop`, a resume, `save-image`, a resume from the Base image, an idle Self-stop and `delete` against a real account, and logs how long the first `up`, each resume and the Self-stop took. It is skipped unless `VULTR_API_KEY`, `DEVCRATE_VULTR_SMOKE_REGION` and `DEVCRATE_VULTR_SMOKE_REPO` (a public repo with a `devcontainer.json`) are set; `DEVCRATE_VULTR_SMOKE_IMAGE` and `DEVCRATE_VULTR_SMOKE_PLAN` are optional. It creates billed resources and deletes them at the end:

```sh
go test -v -run TestLiveSmoke -timeout 60m ./providers/vultr/
```
