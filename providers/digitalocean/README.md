# DigitalOcean Provider

`devcrate-provider-digitalocean` ships with devcrate: it sits next to the `devcrate` executable, where devcrate looks for Providers after `PATH` and `~/.config/devcrate/providers/`. It is a shell script that speaks the Provider contract (`docs/adr/0001-providers-are-external-executables.md`) by running `doctl` and `jq` on your laptop.

An Environment `<name>` is a droplet and a block volume, both named `devcrate-<name>` and tagged `devcrate:<name>`. The volume is mounted at `/var/lib/devcrate` and holds the repo, `/root`, Docker's images and containers, and the devcontainer.

- `up` creates the volume if it is missing, then a droplet with that volume attached (or powers on a droplet that is off).
- `stop` (and the Agent's Self-stop) destroys the droplet and keeps the volume. A stopped Environment costs only the volume's storage.
- `delete` destroys the droplet, the volume and the `devcrate:<name>` tag.
- `status` and `list` read the account by tag; `status` also shows the repo's Base image.
- `save-image` saves the droplet as the repo's Base image and stops the Environment (see below).

```toml
# .devcrate.toml
provider = "digitalocean"
```

```toml
# ~/.config/devcrate/config.toml
[providers.digitalocean]
image = "123456789"   # a snapshot with Docker (see "The Base image")
token = "dop_v1_..."  # the Self-stop token (see below)
```

## Install and log in to doctl

Install `doctl` and `jq` (macOS: `brew install doctl jq`; elsewhere see the [doctl install guide](https://docs.digitalocean.com/reference/doctl/how-to/install/)). Then create a personal access token with full access in the control panel (**API → Generate New Token**) and log in with it:

```sh
doctl auth init
doctl account get # check it works
```

The Provider uses whatever `doctl` is logged in to (`doctl auth switch` picks another context). If `doctl` or `jq` is missing, every verb fails with `digitalocean: doctl is not installed ...` / `jq is not installed ...`.

The first `up` imports devcrate's public key (`~/.config/devcrate/id_ed25519.pub`) into the account as an SSH key named `devcrate`. It is shared by all your Environments, so `delete` leaves it.

## Options

| Option | Default | |
| --- | --- | --- |
| `image` | none, required | Snapshot ID of the image to boot from when the repo has no Base image in `region` (see [The Base image](#the-base-image)), passed to `doctl compute droplet create --image`. Without it `up` fails with doctl's error; there is no fallback to stock Ubuntu. |
| `region` | `nyc3` | Region slug. The volume and every droplet of an Environment live in the region its volume was created in; changing `region` later makes `up` fail. The `image` snapshot must be available there (see `image-action transfer` below). |
| `size` | `s-2vcpu-4gb` | Droplet size slug. 4 GB of memory is enough to build most devcontainers; 1-2 GB droplets run out building Node or Rust images. Change it any time: it applies from the next `up` after a Stop. |
| `volume_size` | `50` | Volume size in GiB, used only when the volume is created. |
| `token` | none | The Self-stop token, returned to devcrate as `self_stop_env = {"DIGITALOCEAN_TOKEN": token}`. Without it `up` fails unless `idle_timeout = 0`. |

`nyc3` and `s-2vcpu-4gb` are defaults rather than required options so that a first Environment needs only `image` and `token`; set `region` to the one nearest to you.

## The Self-stop token

When an Environment is Idle, the Agent on the droplet stops it by destroying its own droplet through the API (`curl -X DELETE https://api.digitalocean.com/v2/droplets/<id>`, the ID read from the droplet's metadata service). It needs a token on the droplet for that, and that token should not be your full-access one:

1. In the control panel, **API → Generate New Token**, choose **Custom Scopes** and select only `droplet: delete` (the control panel may add the read scope it depends on).
2. Put it in your user config as `token` under `[providers.digitalocean]` (not in `.devcrate.toml`, which is committed).

devcrate writes it to a root-only file on the droplet's tmpfs (`/run/devcrate/self-stop.env`) on every `up`; it is never written to the volume.

> **Warning:** DigitalOcean cannot scope a token to one droplet. This token can delete **every droplet in the team**, not just devcrate's. Anyone who gets root on an Environment (including any code you run there) can read it. Use a dedicated team for devcrate if that matters to you, and revoke the token in the control panel if a droplet may have been compromised.

## The Base image

devcrate expects the droplet to come up with Docker (29 or newer) and the devcontainer CLI (0.80.0 or newer, at `/opt/devcontainers`) already installed. `up` installs either one if it is missing, but on every resume of a fresh droplet that costs a minute or more. Save a Ready Environment as the repo's Base image instead (see [Base image](../../README.md#base-image)):

```sh
devcrate save-image          # in the repo's clone, or: devcrate save-image <name>
devcrate save-image --delete # remove it
```

`save-image` powers the droplet off, snapshots it as `devcrate-base-<base name>` (the repo's base Environment name), waits until the snapshot is complete (this can take several minutes), deletes every older snapshot of that name, then destroys the droplet and keeps the volume. A failed snapshot leaves the older Base image in place and the droplet off: `devcrate up` powers it on again. From then on `up` creates the droplets of the repo's Environments from the newest `devcrate-base-<base name>` snapshot, ahead of the `image` option. It includes what your setup scripts installed on the droplet, such as the [GitHub CLI](../../README.md#github-cli), so they skip that on every resume. `devcrate status` shows it; it bills as snapshot storage until `save-image --delete`.

A snapshot is only available in the region it was saved in. When the Environment's `region` is another one, `up` warns and boots from the `image` option instead. To use the Base image there, transfer a copy:

```sh
doctl compute image-action transfer <snapshot-id> --region ams3 --wait
```

### An account-wide image

The `image` option is still required: it is used by a repo's first `up`, before there is a Base image, and in regions the Base image is not in. Without it `up` fails, as DigitalOcean has no stock fallback. Any snapshot with Docker works; the devcontainer CLI is installed on first use. Make one once by hand: create an Ubuntu 24.04 droplet, then

```sh
ssh root@<droplet-ip> 'cloud-init status --wait && curl -fsSL https://get.docker.com | sh'
doctl compute droplet-action power-off <droplet-id> --wait
doctl compute droplet-action snapshot <droplet-id> --snapshot-name devcrate-image --wait
doctl compute snapshot list --resource droplet # the new snapshot's ID is your `image`
doctl compute droplet delete <droplet-id>
```

Do not bake in the Agent: `up` pushes the version matching your `devcrate`.

## Freeing volume space

Docker's images, build cache and stopped containers live on the volume, so they survive a Stop and slowly fill it. Clean up over `devcrate ssh`:

```sh
devcrate ssh <name> -- df -h /var/lib/devcrate
devcrate ssh <name> -- docker system prune      # stopped containers, dangling images, build cache
devcrate ssh <name> -- docker system prune -a   # also every image no container uses
```

`prune` never removes the running devcontainer. Run it while the Environment is Ready (after `up`). To grow the volume instead, resize it in the control panel or with `doctl compute volume-action resize`, then `devcrate ssh <name> -- resize2fs /dev/disk/by-id/scsi-0DO_Volume_devcrate-<name>`.

## Live smoke test

`providers/digitalocean/live_test.go` runs `up`, an Idle Self-stop with your `token`, a resume, `save-image`, an `up` from the Base image, `delete` and `save-image --delete` against a real account, and logs how long each resume took. It is skipped unless `DIGITALOCEAN_ACCESS_TOKEN`, `DEVCRATE_DO_SMOKE_IMAGE`, `DEVCRATE_DO_SMOKE_TOKEN` and `DEVCRATE_DO_SMOKE_REPO` (a public repo with a `devcontainer.json`) are set; it creates billed resources and deletes them at the end:

```sh
go test -v -run TestLiveSmoke -timeout 60m ./providers/digitalocean/
```
