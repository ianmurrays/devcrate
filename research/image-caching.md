# Devcontainer image caching options across a Stop

Research for [#15](https://github.com/ianmurrays/devcrate/issues/15). Feeds [#16](https://github.com/ianmurrays/devcrate/issues/16) (Devcontainer image caching to speed up resume).

Question: how can a devcontainer image survive a Stop, or be rebuilt and pulled quickly, so a resume doesn't rebuild it from scratch? Under the decision in [#3](https://github.com/ianmurrays/devcrate/issues/3) a Stop destroys the droplet and keeps only the block volume, so the root disk and everything Docker keeps on it by default are gone on every resume.

Sources were read on 2026-10-14. Source code was read at these commits:

| Repo | Commit | Date |
|---|---|---|
| devcontainers/cli (package.json `0.89.0`) | `155f8b2` | 2026-10-02 |
| loft-sh/devpod | `5a0efcb` | 2025-11-14 |
| loft-sh/devpod-provider-digitalocean | `cca4f90` | 2026-01-05 |
| digitalocean/openapi | `0dd8470` | 2026-10-13 |

## Summary

- **Recommended for the MVP: option (a), Docker's storage on the volume, with one correction to DevPod's recipe.** Docker Engine 29.0 and later uses the containerd image store on fresh installs, and `data-root` does not move containerd's storage in `/var/lib/containerd`. devcrate installs Docker on a fresh root disk at every resume, so it gets that default every time. Moving only `data-root`, as DevPod's DigitalOcean provider does, would no longer keep images. devcrate must also set containerd's `root`, or turn the containerd store off.
- **With (a), the container survives as well as the image.** On resume, `devcontainer up` finds the existing container by its id-label and runs `docker start`. It builds nothing. `onCreateCommand`, `updateContentCommand` and `postCreateCommand` do not rerun, but `postStartCommand` does. This changes the #4 gist ("postCreateCommand reruns on every resume") and also covers the open item about persisting the devcontainer's home directory.
- **The CLI never checks whether an existing container is stale.** devcrate has to compute its own hash of the configuration and pass `--remove-existing-container` when the hash changes. The rebuild then reuses Docker's layer cache on the volume.
- **Options (b), (c) and (d) all need a registry,** with its own resource, cost and credential. For a single user whose volume already outlives the droplet, they only speed up the first `up` or a second Environment. Defer them. If they are added later, the devcontainer CLI flags cover them, so they are devcrate's concern, not a Provider verb.
- **No published figures exist** for in-region pull speed from DOCR or ECR. Neither is invented below.

## (a) Docker storage on the persistent volume

### What DevPod does

The DigitalOcean provider's user-data script mounts the volume at `/home/devpod` and points Docker's `data-root` at it. It copies the existing `/var/lib/docker` only when the volume is new ([cmd/create.go](https://github.com/loft-sh/devpod-provider-digitalocean/blob/cca4f90f46507f27a690db027bc7f611d8249151/cmd/create.go#L64-L82)):

```sh
mount -o discard,defaults,noatime /dev/disk/by-id/scsi-0DO_Volume_<machineID> /home/devpod
service docker stop
cat > /etc/docker/daemon.json << EOF
{
  "data-root": "/home/devpod/.docker-daemon",
  "live-restore": true
}
EOF
if [ ! -d "/home/devpod/.docker-daemon" ]; then
  mkdir -p /home/devpod/.docker-daemon
  rsync -aP /var/lib/docker/ /home/devpod/.docker-daemon
fi
service docker start
```

The volume is created as ext4 (`FilesystemType: "ext4"`, [pkg/digitalocean/digitalocean.go:41](https://github.com/loft-sh/devpod-provider-digitalocean/blob/cca4f90f46507f27a690db027bc7f611d8249151/pkg/digitalocean/digitalocean.go#L41)).

### The containerd image store breaks that recipe on new Docker installs

- "The containerd image store is the default storage backend for Docker Engine 29.0 and later on fresh installations." Upgraded installs keep the classic overlay2 driver. ([Docker docs: containerd image store](https://docs.docker.com/engine/storage/containerd/)) Docker 29.0.0 is dated 2025-11-10. ([Docker 29 release notes](https://docs.docker.com/engine/release-notes/29/))
- "The `data-root` option does not affect image and container data stored in `/var/lib/containerd` when using the containerd image store. To change the storage location of containerd snapshotters, use the system containerd configuration file" ([Docker docs: daemon, data directory](https://docs.docker.com/engine/daemon/#configure-the-data-directory-location)):

  ```toml
  # /etc/containerd/config.toml
  version = 2
  root = "/mnt/containerd-data"
  ```

- Inference: devcrate gets a fresh root disk and a fresh Docker install on every resume, so it gets the Docker 29+ fresh-install default every time. DevPod's script moves only `data-root`. On such a machine, images and container snapshots would sit in `/var/lib/containerd` on the root disk and be lost at every Stop. I did not run this. The inference rests on the two documented statements above.

There are two ways to fix this:

1. **Keep the containerd store and move both roots onto the volume.** Set `"data-root": "/var/lib/devcrate/docker"` in `/etc/docker/daemon.json` and `root = "/var/lib/devcrate/containerd"` in `/etc/containerd/config.toml`, then restart `containerd` and then `docker`. This keeps the containerd store, and the containerd store is what lets the default `docker` build driver export registry cache (see (c)).
2. **Turn the containerd store off.** Set `"features": {"containerd-snapshotter": false}` together with `data-root`. The docs give the enable form (`"containerd-snapshotter": true`) and say that switching back to the previous storage configuration brings back the hidden overlay2 images ([containerd image store](https://docs.docker.com/engine/storage/containerd/)). They do not give an explicit disable step. Setting it to `false` is my reading, not quoted text. The cost: without the containerd store, the default `docker` driver can't export `registry` cache.

Whichever is chosen, devcrate should check it at every resume with `docker info -f '{{ .DriverStatus }}'`. With the containerd store active it shows `io.containerd.snapshotter.v1` ([containerd image store](https://docs.docker.com/engine/storage/containerd/)). The check catches a Docker upgrade that changes the default again.

An alternative to config files: mount the volume, or a bind mount from it, directly at `/var/lib/docker` and `/var/lib/containerd`. Docker's overlay2 page gives this approach for a different backing filesystem: format it, "mount it at `/var/lib/docker`", and add it to `/etc/fstab` ([overlay2 driver](https://docs.docker.com/engine/storage/drivers/overlayfs-driver/)).

### ext4 on a block volume: compatibility and performance

- overlay2 supports ext4 as a backing filesystem. xfs needs `ftype=1`. ([Select a storage driver](https://docs.docker.com/engine/storage/drivers/select-storage-driver/)) DigitalOcean volumes can be created as ext4 (`--fs-type ext4`, see #3).
- Docker warns: "Make sure you use a dedicated directory for each daemon. If two daemons share the same directory, for example an NFS share, you will experience errors" ([daemon docs](https://docs.docker.com/engine/daemon/#configure-the-data-directory-location)). That holds here, because "You can only attach a volume to one Droplet at a time" ([DO volume limits](https://docs.digitalocean.com/products/volumes/details/limits/)).
- DigitalOcean publishes volume performance by Droplet type ([DO volume limits](https://docs.digitalocean.com/products/volumes/details/limits/)):

  | Droplet type | IOPS | Throughput |
  |---|---|---|
  | Shared | 7,500 | 300 MB/s |
  | Shared (burst) | 10,000 | 450 MB/s |
  | Dedicated | 10,000 | 450 MB/s |
  | Dedicated (burst) | 15,000 | 525 MB/s |

  DigitalOcean publishes no comparable figure for the root disk, so I can't state how much slower builds and container I/O on the volume are than on local disk. It needs measuring.
- Cost: storage is billed at the volume's provisioned size, $0.10/GiB-month (see #3). The volume must be sized for images, build cache and containers on top of the workspace. No source gives a typical devcontainer image size, so no default size is suggested here.

### What breaks if the volume is detached uncleanly

- DigitalOcean: "If you remove a volume from a running Droplet, you may lose data or cause errors on your Droplet. First, turn off the Droplet, or make sure it's not writing to the volume." It recommends checking with `lsof` and unmounting before detaching ([DO: detach a volume](https://docs.digitalocean.com/products/volumes/how-to/detach/)). The page does not say what happens to an attached volume when its Droplet is destroyed, and a Stop is exactly that.
- Docker's overlay2 and daemon pages say nothing about unclean shutdown or how to recover from one. I found no primary source that says how overlay2 or containerd metadata behaves after a hard detach. So I can't claim the setup is safe, or quantify the risk.
- Mitigation using decisions already made: on Idle, #8 has the Agent run `docker stop`, then `sync`, then `self-stop`. With Docker's storage on the volume, the safer sequence also stops the daemons, then unmounts, before the droplet is deleted: `docker stop $(docker ps -q)`, `systemctl stop docker containerd`, `sync`, `umount /var/lib/devcrate`, then `self-stop`. If the unmount fails because something still holds files, that should count as a self-stop failure and be surfaced, as #8 already does for failures.
- `live-restore: true` in DevPod's daemon.json keeps containers running while the daemon is down or restarting. It does nothing for a Stop that destroys the machine.

### What (a) does to the devcontainer lifecycle (devcontainers/cli source)

With Docker's storage on the volume, the stopped **container** survives a Stop, not just the image:

- `devcontainer up` first looks for an existing container by id-label (`findExistingContainer`, [src/spec-node/singleContainer.ts:288-302](https://github.com/devcontainers/cli/blob/155f8b29e1c521bf365e2aebd74f3258a9160718/src/spec-node/singleContainer.ts#L288-L302)). If it finds one, it runs `docker start` (`startExistingContainer`, same file, line 304 onward) and does not build. It builds only when no container exists (lines 31-50).
- `onCreateCommand`, `updateContentCommand` and `postCreateCommand` run only when the marker file `~/.devcontainer/.<hook>Marker` inside the container does not already hold the container's `createdAt`. `postStartCommand` is keyed on `startedAt` ([src/spec-common/injectHeadless.ts:431-440](https://github.com/devcontainers/cli/blob/155f8b29e1c521bf365e2aebd74f3258a9160718/src/spec-common/injectHeadless.ts#L431-L440)). On a resumed container, the create-time hooks are skipped and `postStartCommand` runs. `postAttachCommand` runs every time (line 453).
- The id-label defaults to one derived from the workspace folder path. The default image name is `vsc-<basename>-<sha256(path)>` ([src/spec-node/utils.ts:624-629](https://github.com/devcontainers/cli/blob/155f8b29e1c521bf365e2aebd74f3258a9160718/src/spec-node/utils.ts#L624-L629)). The clone must therefore sit at the same path on the volume after every resume, or devcrate must pass a fixed `--id-label`.
- Consequences for devcrate:
  - The #4 gist "postCreateCommand reruns on every resume under destroy-on-stop" holds only if Docker's storage is on the root disk. With (a), it does not rerun.
  - The container's writable layer, including its home directory, survives a Stop. That covers most of the open map item "Persisting the devcontainer's home directory across a Stop". The exception is anything devcrate deliberately recreates, such as the tmpfs-backed forwarded files from #9.

### What triggers a rebuild with (a)

- **Nothing in the CLI.** When a container exists, `up` doesn't compare `devcontainer.json`, the Dockerfile or Features against it. The `--remove-existing-container` flag ("Removes the dev container if it already exists.", [devContainersSpecCLI.ts:152](https://github.com/devcontainers/cli/blob/155f8b29e1c521bf365e2aebd74f3258a9160718/src/spec-node/devContainersSpecCLI.ts#L152)) is the only way to force a new one. `--build-no-cache` also exists (line 153).
- **devcrate must decide when.** It can store a hash of the build inputs, for example as a `--id-label` value or in a file on the volume. On `up` it recomputes the hash and, if it differs, runs `devcontainer up --remove-existing-container`. DevPod's hash is a reasonable template (see "Staleness detection compared" below).
- **When the CLI does build, Docker's layer cache decides what reruns.** No container means `docker buildx build` runs every time (`buildAndExtendImage`, [singleContainer.ts:180-256](https://github.com/devcontainers/cli/blob/155f8b29e1c521bf365e2aebd74f3258a9160718/src/spec-node/singleContainer.ts#L180-L256)). With the build cache on the volume, unchanged layers are reused. The CLI writes Features into a new temp folder, `<tmpdir>/devcontainercli-<user>/container-features/<version>-<Date.now()>` ([utils.ts:644-646](https://github.com/devcontainers/cli/blob/155f8b29e1c521bf365e2aebd74f3258a9160718/src/spec-node/utils.ts#L644-L646), `createFeaturesTempFolder` at line 635 onward). It resolves Feature references at every build, so a floating tag such as `:1` can pull a new Feature version without any file in the repo changing.

## (b) Push the built image to the cloud's registry, pull on resume

### DigitalOcean Container Registry (DOCR)

- **Plans** ([DOCR pricing](https://docs.digitalocean.com/products/container-registry/details/pricing/), [pricing page](https://www.digitalocean.com/pricing/container-registry)):

  | Plan | Price | Registries | Repositories | Storage | Extra storage |
  |---|---|---|---|---|---|
  | Starter | $0.00/month | 1 | 1 | 500 MiB | Not available |
  | Basic | $5.00/month | 1 | 5 | 5 GiB | $0.02/GiB |
  | Professional | $20.00/month | up to 10 | unlimited | 100 GiB | $0.02/GiB |

  Untagged images count toward the storage quota, so garbage collection matters. Automatic garbage collection is in public preview ([DOCR features](https://docs.digitalocean.com/products/container-registry/details/features/)).
- **Transfer:** neither the DOCR pricing doc nor the pricing page lists a transfer charge. DigitalOcean's bandwidth billing page doesn't cover DOCR, but does say "Inbound transfer to Droplets is free" ([bandwidth billing](https://docs.digitalocean.com/products/billing/bandwidth/)). A push from the droplet is outbound droplet traffic. No primary source gives a DOCR transfer price, so none is stated here.
- **Region:** "You cannot change the datacenter region of a registry after creation" ([DOCR limits](https://docs.digitalocean.com/products/container-registry/details/limits/)). Mirror registries are "available in 11 datacenter regions" ([DOCR features](https://docs.digitalocean.com/products/container-registry/details/features/)).
- **Limits:** no image over 100 GB, 20 GB maximum per layer, registry names globally unique ([DOCR limits](https://docs.digitalocean.com/products/container-registry/details/limits/)).
- **Pull speed:** DigitalOcean publishes no in-region pull throughput figure.
- **Auth from the VM with a scoped credential:**
  - `doctl registry docker-config [--read-write] [--expiry-seconds N]` outputs a Docker `config.json`. "By default this command generates read-only credentials." "By default, the credentials do not expire." The output includes a DigitalOcean API token and "should be treated as sensitive information." ([doctl reference](https://docs.digitalocean.com/reference/doctl/reference/registry/docker-config/))
  - The API behind it is `GET /v2/registries/{registry_name}/docker-credentials?read_write=&expiry_seconds=`. The caller needs `registry:read` and `registry:update` scopes ([OpenAPI spec](https://github.com/digitalocean/openapi/blob/0dd847063b8bc0f1aa2ab7fd0bde3e45f1cc40d4/specification/resources/registry/registries_get_dockerCredentials.yml)).
  - "Registry credentials apply to every registry your DigitalOcean account owns" ([DOCR limits](https://docs.digitalocean.com/products/container-registry/details/limits/)). This is the same shape as the `droplet:delete` finding in #3: the scope covers a resource type across the team, not one registry.
  - Fits devcrate's existing pattern: the laptop mints a read-only credential with a short `--expiry-seconds` on each `up` and pushes it over SSH to a root-only `~/.docker/config.json` on the VM. Pushing from the VM needs `--read-write`.
- **Commands:**

  ```sh
  doctl registry create <name> --subscription-tier basic --region <r>
  doctl registry docker-config --expiry-seconds 3600 > config.json   # read-only, pushed to VM
  # on the VM, after building:
  docker tag <image> registry.digitalocean.com/<name>/<repo>:<hash>
  docker push registry.digitalocean.com/<name>/<repo>:<hash>          # needs --read-write creds
  docker pull registry.digitalocean.com/<name>/<repo>:<hash>          # on resume
  ```

### AWS ECR (for the future AWS Provider)

- **Pricing** ([ECR pricing](https://aws.amazon.com/ecr/pricing/)): the rate table did not load when fetched. The page's Pricing Example 1 says "You are charged for storage at $0.10 per GB for a total of $4/month". New customers get "500 MB per month of storage for your private repositories for one year" on the Free Tier. Data transferred "between Amazon ECR and other services within the same Region e.g., Amazon EC2" is "free of charge (i.e., $0.00 per GB)". Cross-region transfer is charged at internet rates on both sides.
- **Pull speed:** AWS publishes no pull throughput figure. It does publish API rate quotas, for example `GetDownloadUrlForLayer` at 3,000 requests per second per Region and `BatchGetImage` at 2,000 per second, and a 52,000 MiB maximum layer size ([ECR quotas](https://docs.aws.amazon.com/AmazonECR/latest/userguide/service-quotas.html)).
- **Auth from the VM:** an authorization token "is valid for 12 hours" and carries the permissions of the IAM principal that fetched it. Command: `aws ecr get-login-password --region <r> | docker login --username AWS --password-stdin <acct>.dkr.ecr.<r>.amazonaws.com`. The amazon-ecr-credential-helper is an alternative. ([ECR registry auth](https://docs.aws.amazon.com/AmazonECR/latest/userguide/registry_auth.html)) The `AmazonEC2ContainerRegistryPullOnly` managed policy (added 2024-10-10) "grants permission to pull container images" ([ECR managed policies](https://docs.aws.amazon.com/AmazonECR/latest/userguide/security-iam-awsmanpol.html)). Attached to an instance role, it gives the VM pull access with no token pushed at all, which DigitalOcean has no equivalent for.

### What triggers a rebuild with (b)

The registry stores bytes under a tag and nothing else. Staleness is entirely in how devcrate picks the tag. It should use a content hash of the build inputs as the tag (as DevPod does), check whether the tag exists, and pull if it does or build and push if it doesn't.

## (c) Build caching via `@devcontainers/cli` and BuildKit

### CLI flags (devcontainers/cli 0.89.0 source)

`devcontainer build` ([devContainersSpecCLI.ts:572-580](https://github.com/devcontainers/cli/blob/155f8b29e1c521bf365e2aebd74f3258a9160718/src/spec-node/devContainersSpecCLI.ts#L572-L580)):

| Flag | Help text in source |
|---|---|
| `--image-name` | "Image name." Repeatable. |
| `--cache-from` | "Additional image to use as potential layer cache" |
| `--cache-to` | "A destination of buildx cache" |
| `--push` | "Push to a container registry." |
| `--output` | buildx `--output`. Can't be combined with `--push`. |
| `--platform`, `--label`, `--no-cache`, `--buildkit auto\|never` | |

`devcontainer up` also accepts `--cache-from` and `--cache-to` (lines 161-162; `--cache-to` is wired into the build at line 328), plus `--build-no-cache`.

What the CLI passes to Docker (`buildAndExtendImage`, [singleContainer.ts:180-235](https://github.com/devcontainers/cli/blob/155f8b29e1c521bf365e2aebd74f3258a9160718/src/spec-node/singleContainer.ts#L180-L235); the Features-only path in `extendImage`, [containerFeatures.ts:59-93](https://github.com/devcontainers/cli/blob/155f8b29e1c521bf365e2aebd74f3258a9160718/src/spec-node/containerFeatures.ts#L59-L95)):

- With BuildKit it runs `docker buildx build`, with `--push` if `--push` was given, otherwise `--output` if given, otherwise `--load`.
- It passes `--cache-to` through as given.
- Unless `--cache-to` is the inline type, it always adds `--build-arg BUILDKIT_INLINE_CACHE=1`. So every image the CLI builds and pushes carries inline cache metadata.
- It passes `--cache-from` values from the command line, and from `build.cacheFrom` in `devcontainer.json` (string or array), as `--cache-from` (skipped with `--no-cache`). Values go through unchanged, so `type=registry,ref=...` works as well as a plain image ref.
- `--push`, `--platform`, `--output` and `--cache-to` are rejected for Docker Compose configs (lines 743-754).

### BuildKit cache backends

- "The default `docker` driver supports the `inline`, `local`, `registry`, and `gha` cache backends, but only if you have enabled the containerd image store." Other backends need a different driver. ([Cache storage backends](https://docs.docker.com/build/cache/backends/)) This is why option 2 under (a), turning the containerd store off, costs registry cache export.
- Registry cache syntax: `--cache-to type=registry,ref=<registry>/<cache-image>[,mode=max]` and `--cache-from type=registry,ref=<registry>/<cache-image>`. `mode=min` is the default and caches only layers exported into the final image. `mode=max` caches all intermediate layers. ECR needs `image-manifest=true` (with `oci-mediatypes=true`), which "Since BuildKit v0.21 ... is enabled by default." ([Cache storage backends](https://docs.docker.com/build/cache/backends/))
- Inline cache is stored in the pushed image itself. It can be exported with `BUILDKIT_INLINE_CACHE=1` instead of `--cache-to`. Its downside: it "doesn't scale with multi-stage builds as well as the other drivers do" ([Inline cache](https://docs.docker.com/build/cache/backends/inline/)).

### Commands

```sh
# build on the VM, push the image (inline cache included by default) and a separate max-mode cache
devcontainer build --workspace-folder /var/lib/devcrate/<repo> \
  --image-name registry.digitalocean.com/<reg>/<repo>:<hash> \
  --cache-to type=registry,ref=registry.digitalocean.com/<reg>/<repo>:buildcache,mode=max \
  --push

# resume on a fresh root disk: build reuses remote layers instead of rebuilding them
devcontainer up --workspace-folder /var/lib/devcrate/<repo> \
  --cache-from type=registry,ref=registry.digitalocean.com/<reg>/<repo>:buildcache
```

### What triggers a rebuild with (c)

BuildKit's own layer cache keys decide. The registry cache only supplies layers that a cache lookup would otherwise miss, so a change to an instruction or its inputs invalidates that layer and everything after it. The CLI adds no staleness check of its own.

## (d) Prebuilds

### devcontainers spec (containers.dev)

The recommended prebuild command is `devcontainer build --workspace-folder . --push true --image-name <my_image_name>:<optional_image_version>`. The prebuilt image is then referenced from the `image` property. Dev Container configuration and Feature metadata travel in image labels and are "picked up automatically" ([containers.dev prebuild guide](https://containers.dev/guide/prebuild)). The label is `devcontainer.metadata` ([imageMetadata.ts:439](https://github.com/devcontainers/cli/blob/155f8b29e1c521bf365e2aebd74f3258a9160718/src/spec-node/imageMetadata.ts#L439)). The pattern assumes a separate build config and a consumer config that names the image. If the consumer config still lists Features, the CLI builds a Features layer on top (`extendImage`, [containerFeatures.ts:30-57](https://github.com/devcontainers/cli/blob/155f8b29e1c521bf365e2aebd74f3258a9160718/src/spec-node/containerFeatures.ts#L30-L57)). I found no code that skips Features already recorded in the image's metadata label, but I did not trace the whole Features pipeline, so treat that as unverified.

### DevPod

- `devpod build <source> --repository <repo>` builds and pushes `<repo>:devpod-<hash>`. `devpod up --prebuild-repository <repo>`, or `customizations.devpod.prebuildRepository` in `devcontainer.json`, makes `up` look for that tag first. If no match is found, or credentials are missing locally, it builds instead. ([DevPod: prebuild a workspace](https://devpod.sh/docs/developing-in-workspaces/prebuild-a-workspace))
- Source: `buildImage` computes the hash, tries `<repo>:<hash>` in each prebuild repository and uses it if found ([pkg/devcontainer/build.go](https://github.com/loft-sh/devpod/blob/5a0efcbff6610ab114b421f68a890739a452e66b/pkg/devcontainer/build.go#L203-L253)). Without a repository, the Docker driver first looks for a **local** image with that hash and skips the build if it exists ([pkg/driver/docker/build.go:30-48](https://github.com/loft-sh/devpod/blob/5a0efcbff6610ab114b421f68a890739a452e66b/pkg/driver/docker/build.go#L30-L48)). Combined with the DO provider's `data-root` on the volume, this is how DevPod avoids rebuilds on resume.
- Registry cache: `devpod context set-options -o REGISTRY_CACHE=<registry>` ([DevPod: reduce build times with cache](https://devpod.sh/docs/tutorials/reduce-build-times-with-cache)). In source, it sets `--cache-from type=registry,ref=<cache>`. It sets `--cache-to type=registry,ref=<cache>,mode=max,image-manifest=true` only when exporting, with the comment "only export cache on build not up, otherwise we slow down the workspace start time". Without a registry cache it sets `BUILDKIT_INLINE_CACHE=1`. ([pkg/devcontainer/build/options.go:83-101](https://github.com/loft-sh/devpod/blob/5a0efcbff6610ab114b421f68a890739a452e66b/pkg/devcontainer/build/options.go#L83-L101)) The docs tell Docker-provider users to enable `containerd-snapshotter` for this.

### GitHub Codespaces

- A prebuild runs setup "up to and including any `onCreateCommand` and `updateContentCommand` commands". "No `postCreateCommand` commands are run during the creation of a prebuild." Prebuild storage "is billed in the same way as storage of active or stopped codespaces." ([About prebuilds](https://docs.github.com/en/codespaces/prebuilding-your-codespaces/about-github-codespaces-prebuilds))
- Triggers: "Every push" (default), "On configuration change", or "Scheduled". "On configuration change" covers `.devcontainer/devcontainer.json` and the Dockerfile named in its `build.dockerfile`. Changes to `devcontainer.json` files in subdirectories of `.devcontainer` do not trigger it. Prebuilds are created per region (each billed) and 1 to 5 versions are retained (default 2). ([Configuring prebuilds](https://docs.github.com/en/codespaces/prebuilding-your-codespaces/configuring-prebuilds))

### Gitpod (now Ona)

- Prebuilds run the Dev Container build, `initializeCommand`, `onCreateCommand`, `updateContentCommand` and tasks with the `prebuild` trigger. They skip `postCreateCommand`, `postStartCommand` and `postAttachCommand`. Then "Ona snapshots the Dev Container filesystem". Selection is best-effort, uses the most recent successful prebuild, and prebuilds expire after 5 days. ([Ona: prebuilds](https://ona.com/docs/ona/projects/prebuilds))
- The separate Dev Container image cache keys on the contents of `devcontainer.json`, the contents of any referenced Dockerfile, and the digest of any image referenced directly in `devcontainer.json`. "Base images referenced in `FROM` instructions are not monitored directly." Only the initial build at environment creation pushes. Rebuilds only pull. On AWS runners, images go to ECR in the runner's region and expire after 30 days. Docker Compose configs are not supported. ([Ona: Dev Container image cache](https://ona.com/docs/ona/runners/devcontainer-image-cache))

## Staleness detection compared

| Tool | What decides "rebuild needed" | Inputs |
|---|---|---|
| devcontainers/cli | Existing container found by id-label means no build. Otherwise Docker's layer cache. | No hash of its own. Features re-resolved on every build. |
| DevPod | Tag `devpod-<hash>`, looked up locally, then in prebuild repositories | Target arch + `devcontainer.json` trimmed to build-relevant fields (`image`, `dockerfile`, `context`, `build`, `features`, `overrideFeatureInstallOrder`, `name`) + Dockerfile content + hash of the build-context files the Dockerfile references, honouring `.dockerignore` ([pkg/devcontainer/config/prebuild.go:18-84](https://github.com/loft-sh/devpod/blob/5a0efcbff6610ab114b421f68a890739a452e66b/pkg/devcontainer/config/prebuild.go#L18-L84)). Features are hashed as written (references and options), not as resolved digests. |
| Ona | Config hash cache key | `devcontainer.json` + referenced Dockerfile + digest of a directly referenced image. Not `FROM` images. |
| Codespaces | Prebuild trigger "On configuration change" | `.devcontainer/devcontainer.json` + its `build.dockerfile`. |

None of the three hashes catches a floating Feature tag or a `FROM` base image moving to a new version. Each one needs a forced rebuild for that. Codespaces "Every push" and "Scheduled" triggers and Ona's 30-day expiry are the time-based backstops.

## Implications for devcrate (input to #16)

1. **MVP: option (a), corrected.** Put both `data-root` and containerd's `root` on the volume (or mount both directories from it), and check `docker info` on every resume. The result: no registry, no extra resource or credential, and resume is a `docker start` of the existing container. The cost is volume size at $0.10/GiB-month.
2. **devcrate owns staleness.** It computes a DevPod-style hash over arch, the build-relevant `devcontainer.json` fields, the Dockerfile, and the context files the Dockerfile references, and stores it with the Environment. When the hash changes it runs `devcontainer up --remove-existing-container`. Docker's cache on the volume keeps that rebuild incremental. A user-facing `--rebuild` covers floating Feature tags and base images.
3. **The Self-stop sequence should stop dockerd and containerd and unmount** before deleting the droplet, because the volume is then carrying live Docker metadata and DigitalOcean warns against detaching while writes are possible.
4. **Defer registry push/pull, registry cache and prebuilds.** On one volume-backed Environment they only help the first `up`. If added later, they are flags on `devcontainer build`/`up` (`--image-name`, `--push`, `--cache-from`, `--cache-to`) plus a credential pushed over SSH. That is devcrate's concern, with the registry ref in config. ADR 0001's Provider verbs need no change. The user creates and pays for the registry (DOCR Basic is $5.00/month).
5. **Revise the #4 line:** with (a), `postCreateCommand` does not rerun on resume. Only `postStartCommand` and `postAttachCommand` do.
6. **Measure before promising numbers:** volume vs root-disk build time, and `docker start` resume time. No primary source publishes either.
