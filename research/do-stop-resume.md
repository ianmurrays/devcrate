# Stopping and resuming a DigitalOcean environment without paying for the droplet

Research for [#3](https://github.com/ianmurrays/devcrate/issues/3), child of map [#1](https://github.com/ianmurrays/devcrate/issues/1). Researched 2026-10-13 from DigitalOcean docs, the DigitalOcean OpenAPI spec (`digitalocean/openapi`), the `doctl` source (`digitalocean/doctl` @ `857c99b`), `loft-sh/devpod-provider-digitalocean` @ `cca4f90` and `loft-sh/devpod` @ `5a0efcb`. No DigitalOcean resources were created; nothing here was measured.

## Answer

- **Stop = destroy the droplet. Keep state on a block storage volume that outlives it.** Resume = create a new droplet with `--volumes <id>`. This is exactly what DevPod's DigitalOcean provider does.
- A snapshot-based stop costs less per GB stored, but it needs a running droplet to orchestrate it (power off, snapshot, delete). A VM cannot power itself off and then take the snapshot, so a self-stop would have to take a live snapshot (DO warns about consistency) and keep paying for the droplet while the snapshot runs at about 1 to 3 minutes per GB of used disk. Then it has to delete itself. The volume path is a single `DELETE /v2/droplets/$ID`, and billing ends at destroy.
- **Self-stop is possible.** The droplet reads its own ID from the metadata service, which needs no token, and calls the delete endpoint with a custom-scoped token that holds only `droplet:delete`. The catch: scopes cover a resource *type*, not one resource, so that token can delete **every droplet in the team**.

## 1. What billing actually does

| Resource | Price | Billed while | Source |
|---|---|---|---|
| Droplet (bundled CPU plans) | per second, minimum 60 s or $0.01 | from creation until **destroyed**. "You are still billed for bundled-plan CPU Droplets that are powered off." | [Droplet pricing](https://docs.digitalocean.com/products/droplets/details/pricing/) |
| Volume | $0.10 per GiB-month, on **provisioned** size | "Charges accrue hourly for as long as the volume exists." Billed "whether or not they are attached to a Droplet." Size 1 GiB to 16,384 GiB. | [Volume pricing](https://docs.digitalocean.com/products/volumes/details/pricing/) |
| Snapshot | $0.06 per GB-month (droplet), $0.06 per GiB-month (volume), minimum $0.01 | until deleted. "Snapshots are retained indefinitely until manually deleted." | [Snapshot pricing](https://docs.digitalocean.com/products/snapshots/details/pricing/), [features](https://docs.digitalocean.com/products/snapshots/details/features/) |
| Reserved IPv4 | free when assigned, $5.00/month ($0.01/hr) when unassigned | while reserved | [Reserved IP pricing](https://docs.digitalocean.com/products/networking/reserved-ips/details/pricing/) |

The snapshot size you pay for is the compressed used data, not the disk size. The API defines `size_gigabytes` as "The billable size of the snapshot in gigabytes" and `min_disk_size` as "The minimum size in GB required for a volume or Droplet to use this snapshot" (`specification/resources/snapshots/models/snapshots_base.yml`). The API's example snapshot has `size_gigabytes: 2.34` against `min_disk_size: 25`. `doctl compute snapshot` describes the field as "The compressed, billable size of the snapshot" (`commands/snapshots.go`). Snapshots "are not incremental. Each snapshot is a full disk image" ([snapshot limits](https://docs.digitalocean.com/products/snapshots/details/limits/)).

Reference plan prices ([droplet pricing page](https://www.digitalocean.com/pricing/droplets), Basic, regular CPU): 2 vCPU/4 GiB/80 GiB SSD is $24/mo ($0.03571/hr), 4 vCPU/8 GiB/160 GiB is $48/mo ($0.07143/hr), 8 vCPU/16 GiB/320 GiB is $96/mo ($0.14286/hr).

## 2. Options compared

### A. Destroy the droplet, keep a volume (recommended)

- **Stop:** sync, unmount, then `DELETE /v2/droplets/$ID`. A plain droplet delete does not delete the volume. The destroy docs say associated volumes and snapshots are destroyed only if you opt in ([destroy how-to](https://docs.digitalocean.com/products/droplets/how-to/destroy/)). The API's selective-destroy endpoint states that "Any associated resource not included in the request will remain and continue to accrue [charges]" (`droplets_destroy_withAssociatedResourcesSelective.yml`).
- **Resume:** `POST /v2/droplets` with `volumes: [<id>]`. The volumes "must not already be attached to an existing Droplet" (`droplet_create.yml`). I found no DigitalOcean doc that says outright that destroying a droplet detaches its volume. DevPod's provider depends on it: its `Stop` only deletes the droplet, and its `Start` re-creates the droplet with the same volume ID (`pkg/digitalocean/digitalocean.go` L31-89). Treat it as confirmed in practice but not documented.
- **Stopped cost:** $0.10 × provisioned GiB per month. Example: a 50 GiB volume costs $5.00/month whether attached or not.
- **Running cost:** droplet plus volume. Example: 4 vCPU/8 GiB at $48/mo plus a 50 GiB volume at $5.00/mo, about $53/mo if it runs all month.
- **Limits:** up to 15 volumes per droplet and 5,000 per account ([volume limits](https://docs.digitalocean.com/products/volumes/details/limits/)). A volume belongs to one region and attaches only to a droplet in that region (the attach action takes "the region where the volume is located", `volumeActions_post.yml`), so resume must use the same region.
- **What is lost on stop:** the droplet's root disk. DevPod handles this by mounting the volume at `/home/devpod` and moving Docker's `data-root` to `/home/devpod/.docker-daemon` in its user-data script. Container images and layers therefore survive a stop (`cmd/create.go` `GetInjectKeypairScript`). Anything on the root disk, such as the OS packages and devcrate's agent binary, has to be reinstalled on every resume by cloud-init or baked into a custom base image. This ties into the map's open item "cloud-init vs custom image".

### B. Power off, snapshot, destroy (snapshot only)

- **Stop:** `shutdown` or `power_off`, then the `snapshot` action, wait for it to finish, then delete. The docs say "Creating a snapshot or backup takes about 1 to 3 minutes per GB of used disk space on the Droplet" and that it varies with load ([support article](https://docs.digitalocean.com/support/how-long-will-my-backup-or-snapshot-take-to-complete/)). The droplet is billed for that whole time.
- **Resume:** create a droplet with `--image <snapshot-id>`, on a plan whose disk is at least the snapshot's `min_disk_size`. You cannot resume onto a plan with a smaller disk.
- **Stopped cost:** $0.06 × compressed used GB. Example: 10 GB of compressed data costs $0.60/month, against $5.00/month for a 50 GiB volume. Snapshot storage is cheaper.
- **Problems:**
  - It cannot be fully self-driven. Once the VM powers itself off, nothing is left to start the snapshot. A live snapshot is possible, but "Creating a snapshot while a Droplet is on may result in performance degradation and compromise data consistency" ([snapshot how-to](https://docs.digitalocean.com/products/snapshots/how-to/snapshot-droplets/)). `doctl` also says "We recommend that you power off the Droplet before taking a snapshot to ensure data consistency" (`commands/droplet_actions.go`).
  - Stop takes minutes to tens of minutes, not seconds. If the snapshot fails, the droplet must not be deleted, so the stop flow needs action polling and error handling.
  - Each stop writes a full new image, and the previous one has to be deleted.
  - It needs more scopes: see section 4.

### C. Hybrid (lead, not a decision)

Use a snapshot or custom image once, as a **base image** with the tooling and agent preinstalled. Keep per-environment state on a volume (option A). This keeps stop instant and cheap, and resume skips reinstalling the tooling. Image strategy belongs to the "agent binary and base tooling" ticket.

## 3. Resume time

No DigitalOcean doc I found gives a figure or SLA for droplet creation time, or for how long creating a droplet from a snapshot takes. The figures that turned up are community forum anecdotes from 2015 and later, so they are not quoted here. **Measure it** with a timed `doctl compute droplet create --wait` in the target region, once with a stock image and a volume and once from a snapshot, before promising a resume time in the spec.

## 4. Self-stop from the VM: identity and credentials

**Identity:** the metadata service is at the link-local address `169.254.169.254`, top-level `/metadata/v1/`, and lists `id`, `user-data` and other properties ([access metadata](https://docs.digitalocean.com/products/droplets/how-to/access-metadata/)). So `curl -s http://169.254.169.254/metadata/v1/id` returns the droplet's own ID, with no token. The docs do not describe any authentication on this service.

**Scopes needed** (from the `security` blocks in the OpenAPI spec):

| Call | Scope |
|---|---|
| `DELETE /v2/droplets/{id}` | `droplet:delete` |
| `DELETE /v2/droplets?tag_name=` | `droplet:delete` (the description also says it "requires `tag:read` scope") |
| `POST /v2/droplets/{id}/actions` with `shutdown`/`power_off`/`power_on` | `droplet:update` |
| `POST /v2/droplets/{id}/actions` with `snapshot` | `droplet:update` + `image:create` |
| `GET /v2/droplets` | `droplet:read` |
| `POST /v2/droplets` | `droplet:create` (+ `block_storage:read` when passing `volumes`) |
| `POST /v2/volumes` | `block_storage:create` |
| volume attach/detach | `block_storage_action:create` |
| `DELETE /v2/snapshots/{id}` | `snapshot:delete` / `image:delete` |

So the **minimum token for option A self-stop is `droplet:delete` alone**. Option B self-stop needs `droplet:update`, `image:create`, `droplet:delete`, and read access to poll the action.

**Token properties** ([create a personal access token](https://docs.digitalocean.com/reference/api/create-personal-access-token/), [scopes](https://docs.digitalocean.com/reference/api/scopes/)):
- Custom scopes are "granular scopes that grant access to a specific resource type and action". There is no way to bind a token to one droplet, tag or project. **A `droplet:delete` token can delete every droplet the team owns.**
- Tokens have an expiration you choose. "You cannot edit the scope of a token after creation", but you can regenerate the token. A missing scope returns 403.
- A custom-scoped token keeps its original scopes when the team role later gains permissions, and loses any scope the role later loses.

**How DevPod does it:** the provider's `agent.exec.shutdown` is `${DO_PROVIDER} stop` (`hack/provider/provider.yaml`). When the agent daemon on the VM sees inactivity, it runs that command with `provider.ToEnvironment(..., workspace.Options, ...)` (`cmd/agent/daemon.go` `runShutdownCommand`, `pkg/driver/custom/custom.go` `ToEnvironWithBinaries`). The workspace options include `TOKEN`. In other words, DevPod copies the user's own token, typically full access, onto the VM. `stop` then calls `Droplets.Delete` and keeps the volume.

**Recommendations for devcrate:**
1. Mint a separate custom-scoped token with only `droplet:delete` and a short expiry for VM self-stop. Never ship the laptop's `doctl` token, which is usually full access.
2. **Do not put the token in user-data.** `user-data` is served by the metadata service, which needs no authentication, so any process on the droplet that can reach `169.254.169.254` can read it. Push the token over SSH after boot into a root-only file.
3. To limit the blast radius, consider a dedicated DO team for devcrate, so `droplet:delete` cannot touch other droplets. The alternative is for the VM not to delete itself at all: it signals stop and the laptop deletes. That fails the "stops billing even when the laptop is offline" goal, and a powered-off droplet still bills.
4. Before deleting, the agent should `sync` and unmount the volume, or at least stop containers. Deleting a running droplet is effectively pulling the power on the volume's filesystem.

## 5. doctl commands

Volume path (A):

```sh
# create (once)
doctl compute volume create devcrate-<env> --region fra1 --size 50GiB \
  --fs-type ext4 --fs-label devcrate --tag devcrate
doctl compute droplet create devcrate-<env> --region fra1 --size s-4vcpu-8gb \
  --image ubuntu-24-04-x64 --volumes <volume-id> --ssh-keys <fp> \
  --user-data-file cloud-init.yaml --tag-names devcrate --wait

# stop (from laptop; on the VM, the same as a DELETE with the droplet:delete token)
doctl compute droplet delete <droplet-id> --force

# resume
doctl compute droplet create devcrate-<env> ... --volumes <volume-id> --wait

# destroy the environment for good
doctl compute volume delete <volume-id> --force
```

On the VM, self-stop without doctl:

```sh
ID=$(curl -s http://169.254.169.254/metadata/v1/id)
curl -fsS -X DELETE -H "Authorization: Bearer $(cat /root/.devcrate/stop-token)" \
  "https://api.digitalocean.com/v2/droplets/$ID"
```

Snapshot path (B):

```sh
doctl compute droplet-action power-off <droplet-id> --wait   # or `shutdown`
doctl compute droplet-action snapshot <droplet-id> --snapshot-name devcrate-<env>-<ts> --wait
doctl compute droplet delete <droplet-id> --force
# resume
doctl compute droplet create devcrate-<env> --image <snapshot-id> --size <plan with disk >= min_disk_size> --region fra1 --wait
doctl compute snapshot delete <old-snapshot-id> --force
```

Flag sources: `doctl` `commands/volumes.go` (`create` with `--size`, `--region`, `--snapshot`, `--fs-type`, `--fs-label`, `--tag`; `snapshot <volume-id> --snapshot-name`), `commands/droplets.go` (`create --volumes`, `--user-data-file`, `--tag-names`, `--wait`; `delete --force/-f`, `--tag-name`), `commands/droplet_actions.go` (`snapshot <droplet-id> --snapshot-name --wait`), `commands/snapshots.go` (`list`, `get`, `delete`). `commands/droplet_actions.go` also defines `shutdown <droplet-id>` and `power-off <droplet-id>`, and `volume delete` and `snapshot delete` both take `--force/-f`.

## 6. Other things that change on resume

- **IP address:** a new droplet gets a new public IP. A reserved IPv4 would keep the address stable, but it costs $5.00/month while unassigned, which means the whole time the environment is stopped. That is as much as a 50 GiB volume. The cheaper fix is to rewrite the SSH config entry on every resume.
- **SSH host key:** a fresh root disk means a new host key on every resume unless the key is kept on the volume, or `known_hosts` is updated by devcrate.

## Open items

- Measure resume time (section 3).
- Confirm that volume detach on droplet destroy is immediate enough for an instant re-create. DevPod's `Start` re-creates without waiting and evidently works, but no doc states it.
