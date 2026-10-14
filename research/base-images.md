# Base images on DigitalOcean: options, cost and boot time

Research for [#13](https://github.com/ianmurrays/devcrate/issues/13), child of map [#1](https://github.com/ianmurrays/devcrate/issues/1). Researched 2026-10-14 from DigitalOcean docs, the DigitalOcean OpenAPI spec (`digitalocean/openapi` @ `0dd8470`), the `doctl` source (`digitalocean/doctl` @ `857c99b`), the DigitalOcean 1-Click build repo (`digitalocean/droplet-1-clicks` @ `929aeae`), the Packer DigitalOcean plugin (`digitalocean/packer-plugin-digitalocean` @ `9598652`) and its docs, `loft-sh/devpod` @ `5a0efcb`, `loft-sh/devpod-provider-digitalocean` @ `cca4f90`, `loft-sh/devpod-provider-aws` @ `0da5d6e`, `coder/registry` @ `e53a852`, `devcontainers/cli` @ `155f8b2`, Ona (Gitpod) docs and AWS docs. No DigitalOcean resources were created; nothing here was measured.

## Answer

- **Use a droplet snapshot as the base image, built by Packer's `digitalocean` builder** (or the equivalent four `doctl` calls). Packer's DigitalOcean builder produces exactly that: a droplet snapshot (image `type: snapshot`). Reference it at create time by its numeric image ID: `doctl compute droplet create ... --image <id>`.
- **Cost is small:** $0.06 per GB-month on the snapshot's size, with no charge for adding it to more regions. Region scope: a snapshot is usable only in the regions it has been added to (`doctl compute image-action transfer <id> --region <r>`).
- **An uploaded custom image adds restrictions and no benefit here:** BIOS boot only, no IPv6, DHCP networking, no automatic monitoring. It also needs the file hosted at a URL first.
- **The Marketplace Docker 1-Click covers only Docker.** It has no Node and no devcontainer CLI. It also enables UFW with 2375/2376 open. It is what DevPod's DigitalOcean provider defaults to, and it can serve as a Packer source image, but it does not replace a devcrate image.
- **Boot and install time:** DigitalOcean publishes no create-time figure for a stock image, a snapshot or a custom image. Docker, Node.js and `devcontainers/cli` publish no install-time figure. Both have to be measured.
- **The idea fits more than one Provider.** AWS's AMI is the same concept: regional, copied per region, stored as snapshots, and passed as `ImageId` at launch. Ona (Gitpod) ships its own AMI. DevPod and Coder default to stock or 1-Click images and install at boot.

## 1. The three options

| | A. Droplet snapshot | B. Uploaded custom image | C. Marketplace Docker 1-Click |
|---|---|---|---|
| What it is | Image of a droplet's disk you took yourself (`type: snapshot`) | VM disk file you import (`type: custom`) | DigitalOcean-published public image with Docker preinstalled |
| How it is built | Create droplet, install, power off, snapshot, delete (Packer `digitalocean` builder does this) | Build a disk file elsewhere (e.g. Packer `qemu`), host it at a URL, `doctl compute image create` | Already built by DigitalOcean (Packer templates in `droplet-1-clicks`) |
| Contains devcrate prerequisites | Whatever you install | Whatever you install | Docker (+ Compose) only |
| Storage cost | $0.06/GB-month | $0.06/GB-month | none |
| Extra regions | free; `image-action transfer` | free; `image-action transfer` | all regions |
| `--image` value | numeric ID | numeric ID | slug (see §1C) |
| Notable limits | `min_disk_size` = build droplet's disk; usable only in regions it was added to | < 100 GB uncompressed, ext3/ext4, BIOS only, no IPv6, DHCP, cloud-init ≥ 0.7.7 | no Node, no devcontainer CLI; UFW on |

The `--image` flag on `doctl compute droplet create` takes "An ID or slug specifying the image to use to create the Droplet" (`commands/droplets.go`). The API field is "The image ID of a public or private image or the slug identifier for a public image" (`droplets/models/droplet_create.yml`). So private images (A and B) are always referenced by **numeric ID**, and only public images have slugs.

### A. Droplet snapshot (recommended)

**Build with doctl** (flags from `commands/droplets.go`, `commands/droplet_actions.go`, `commands/images.go`):

```sh
doctl compute droplet create devcrate-image-build --region fra1 --size s-1vcpu-1gb \
  --image ubuntu-24-04-x64 --ssh-keys <fp> --user-data-file install.sh --wait
# wait for cloud-init: ssh root@<ip> cloud-init status --wait
doctl compute droplet-action shutdown <droplet-id> --wait      # or power-off
doctl compute droplet-action snapshot <droplet-id> --snapshot-name devcrate-base-<ts> --wait
doctl compute droplet delete <droplet-id> --force
doctl compute image list-user                                   # find the new image ID
doctl compute image-action transfer <image-id> --region nyc3 --wait   # per extra region
```

API equivalents: `POST /v2/droplets`, `POST /v2/droplets/{id}/actions` with `{"type":"snapshot","name":...}` ([snapshot how-to](https://docs.digitalocean.com/products/snapshots/how-to/snapshot-droplets/)), `POST /v2/images/{id}/actions` with `{"type":"transfer","region":...}` (`images/imageActions_post.yml`, scope `image:update`).

**Build with Packer:** the [DigitalOcean builder](https://developer.hashicorp.com/packer/integrations/digitalocean/digitalocean/latest/components/builder/digitalocean) needs `api_token`, `image`, `region`, `size`. It accepts `snapshot_name`, `snapshot_regions` (extra regions to distribute to), `snapshot_timeout` (default 60 min) and `transfer_timeout` (default 30 min). In the plugin source, the build steps are create droplet, connect, provision, `stepShutdown`, `stepPowerOff`, `stepSnapshot`, and `stepCreateDroplet.Cleanup` then destroys the build droplet (`builder/digitalocean/builder.go`, `step_create_droplet.go`). That matches DigitalOcean's advice to power off before snapshotting. DigitalOcean builds its own 1-Click images with this builder (`droplet-1-clicks/docker-22-04/template.json`).

**Cost:** "Snapshots are charged at $0.06 per GB per month for Droplets", minimum charge $0.01, "Pricing is based on the size of the snapshot", and "There is no additional charge for making a snapshot available in multiple regions" ([snapshot pricing](https://docs.digitalocean.com/products/snapshots/details/pricing/)). The API calls `size_gigabytes` the billable size, and `doctl` calls it "The compressed, billable size of the snapshot" (see the #3 write-up). DigitalOcean publishes no size for a stock Ubuntu image with Docker, Node and the devcontainer CLI installed. Illustration only: a 5 GB snapshot would cost $0.30/month.

**Region scope:** "By default, a Droplet snapshot is only available in the region where it was created. If you want to use a snapshot to create Droplets in other regions, you need to add the snapshot to those regions first." Adding regions "doesn't cost more" and "may take several minutes" ([add snapshots to regions](https://docs.digitalocean.com/products/snapshots/how-to/change-regions/)). Since devcrate's volume pins an environment to one region anyway (#3), one image per region in use is enough.

**Limits:**
- **Disk size:** the image's `min_disk_size` is "The minimum disk size in GB required for a Droplet to use this image" (`images/models/image.yml`). It comes from the build droplet's disk, so build on the smallest plan (the 1-Click templates use `s-1vcpu-1gb`) and the image boots on any larger plan.
- **Volumes are not captured:** "Creating a snapshot of a Droplet does not capture volumes attached to the Droplet" ([snapshot limits](https://docs.digitalocean.com/products/snapshots/details/limits/)). Build on a droplet with no volume.
- **Count and expiry:** DigitalOcean publishes no count limit for droplet snapshots (the only published count is 25 snapshots per *volume*), and "Snapshots are retained indefinitely until manually deleted" ([features](https://docs.digitalocean.com/products/snapshots/details/features/)).
- **Capacity:** "You cannot create Droplets in certain datacenters due to limited capacity. If you have snapshots in a limited capacity datacenter, transfer them to another datacenter to create Droplets from them" ([support article](https://docs.digitalocean.com/support/why-cant-i-create-a-droplet-from-a-snapshot/)).
- **Staleness:** the image freezes OS packages and tool versions at build time. That is a maintenance cost, not a platform limit: devcrate needs a rebuild command or a documented Packer template.

### B. Uploaded custom image

**Build:** produce a disk image outside DigitalOcean, host it at a URL, then import it:

```sh
doctl compute image create devcrate-base --image-url https://<host>/devcrate-base.qcow2.gz \
  --region fra1 --image-distribution Ubuntu --image-description "..." --tag-names devcrate
```

Flags from `commands/images.go` (`--image-url` and `--region` required). API: `POST /v2/images` with `name`, `url`, `region` required, scope `image:create` (`images/images_create_custom.yml`). Packer's `digitalocean-import` post-processor automates this: it uploads the file to a Spaces bucket (`spaces_key`, `spaces_secret`, `spaces_region`, `space_name`, all required) and imports it into `image_regions` (`post-processor/digitalocean-import/post-processor.go`).

**Cost:** "Custom images are free to upload and charged at $0.06 per GB per month to store. There is no additional charge to add an image to additional regions" ([custom image pricing](https://docs.digitalocean.com/products/custom-images/details/pricing/)). This is the same rate as a snapshot.

**Region scope:** you can create droplets from it "as long as they are in the same datacenter". To use it elsewhere, run the same `doctl compute image-action transfer` ([add regions](https://docs.digitalocean.com/products/custom-images/how-to/add-regions/)).

**Requirements and limits** ([upload how-to](https://docs.digitalocean.com/products/custom-images/how-to/upload/), [limits](https://docs.digitalocean.com/products/custom-images/details/limits/)):
- Formats: raw (`.img`, MBR or GPT), qcow2, VHDX, VDI, VMDK, optionally gzip or bzip2. No ISO.
- "Images must be less than 100 GB uncompressed." Filesystem ext3 or ext4.
- cloud-init 0.7.7 or later (or another listed init), with `ConfigDrive` before `NoCloud` in the data source order. `sshd` must run on boot.
- "UEFI boot is not supported. Custom images must boot using BIOS."
- "You cannot use IPv6 with Droplets created from custom images." Networking comes from DHCP, so a Cloud Firewall needs an outbound UDP 67 rule.
- "You cannot enable monitoring automatically during Droplet creation when using a custom image."
- An SSH key is required at create time.
- No published count limit or expiry.

**Verdict:** a custom image gives devcrate nothing that a snapshot does not, and it adds hosting, format and networking constraints. It only makes sense for an image built off-platform, for example one shared with another Provider.

### C. Marketplace Docker 1-Click

**What it contains:** the listing says "Docker Version latest, OS Ubuntu 22.04" ([marketplace listing](https://marketplace.digitalocean.com/apps/docker)). The build template (`droplet-1-clicks/docker-22-04/template.json`) starts from `ubuntu-22-04-x64`, runs a full upgrade, and installs `docker-ce` from Docker's apt repository (`common/scripts/010-docker.sh`). It also installs Docker Compose (`011-docker-compose.sh`) and enables UFW with `ufw limit ssh`, `ufw allow 2375/tcp` and `ufw allow 2376/tcp` (`014-ufw-docker.sh`). The template's `application_version` variable says `23.0.6`, but that is only a label: the script installs whatever `docker-ce` version apt resolves at build time. Node.js and the devcontainer CLI are not included.

**Reference at create:** the listing's API example and its "Create Docker Droplet" link use the slug `docker-20-04`. The same page says Ubuntu 22.04, and the source directory is `docker-22-04`. I could not resolve this mismatch without an `image:read` token. `doctl compute image list-application` shows the live slug. DevPod's DigitalOcean provider defaults `DISK_IMAGE` to `docker-20-04` (`hack/provider/provider.yaml`).

**Cost and region:** no image storage charge and no listed price on the page. Marketplace images are public, so there is no transfer step.

**Verdict:** devcrate would still have to install Node and the devcontainer CLI on every resume, and would have to manage the UFW rules. It is a reasonable `image` source for the Packer build, but it does not make a custom image unnecessary.

## 2. Boot time and install time

**Droplet create time:** DigitalOcean's docs publish no figure or SLA for how long a droplet takes to create, from a stock image, a snapshot or a custom image. This matches the #3 finding. The only figures are community forum anecdotes, which are not quoted here. The only related figure DigitalOcean publishes is for *taking* a snapshot, about 1 to 3 minutes per GB of used disk (#3). That cost falls on the image build, not on resume.

AWS documents one mechanism worth testing for on DigitalOcean: "For volumes created from snapshots, the data blocks must be downloaded from Amazon S3 to the new volume … the volume being initialized might experience increased I/O latency" ([EBS initialization](https://docs.aws.amazon.com/ebs/latest/userguide/initalize-volume.html)). So on AWS, booting from a custom AMI can be slower on first disk access than the API's "running" state suggests. DigitalOcean documents nothing equivalent either way.

**Install time for Docker + Node ≥20 + `@devcontainers/cli` on fresh Ubuntu:** none of Docker's Ubuntu install docs, NodeSource's `distributions` README or the `devcontainers/cli` README publish an install time. One finding that affects the comparison: the devcontainer CLI now has a standalone installer that "downloads a bundled Node.js runtime, so no pre-installed Node.js is required" (`devcontainers/cli` README, `scripts/install.sh`). The npm route needs "Python and C/C++ installed to build one of the dependencies". So the stock-image path is Docker's apt install plus one installer script, not three separate package installs.

**How to get the numbers:** time `doctl compute droplet create --wait` in the target region with `ubuntu-24-04-x64` and with the snapshot. Then, on the VM, `cloud-init analyze show` / `cloud-init analyze blame` gives per-stage timings, and devcrate's `setup.log` on the volume can timestamp each install step. Doing this on the first real `up` produces the comparison the spec needs.

## 3. How other tools handle base images for cloud VMs

- **DevPod, DigitalOcean provider:** `DISK_IMAGE` option, default `docker-20-04` (the Docker 1-Click), with stock distro slugs as suggestions (`hack/provider/provider.yaml`). DevPod does not need Node on the VM because its devcontainer engine is written in Go.
- **DevPod, AWS provider:** `AWS_AMI` option, default empty. When empty, `GetDefaultAMI` picks the newest Amazon- or self-owned image whose description matches `Canonical, Ubuntu, 22.04 LTS*` for the instance's architecture (`pkg/aws/aws.go`).
- **DevPod agent, any provider:** if `docker` is missing, `cmd/agent/workspace/up.go` runs an embedded copy of Docker's `get.docker.com` script (`scripts/install_docker.sh`). So DevPod installs at boot and treats a prebaked image as an optional speed-up.
- **Coder, DigitalOcean template:** `droplet_image` parameter, default `ubuntu-22-04-x64`. It uses a `digitalocean_volume` for home and creates the droplet with `count = start_count`, so the droplet is destroyed on stop and the agent is installed by cloud-init (`registry/coder/templates/digitalocean-linux/main.tf`, `cloud-config.yaml.tftpl`). This is the same shape as devcrate (#3).
- **Coder, AWS template:** `data "aws_ami"` picks the newest Canonical Ubuntu 20.04 image, owner `099720109477` (`registry/coder/templates/aws-linux/main.tf`). Coder's docs advise baking dependencies into images: "Templates with complex infrastructure, large images, or lengthy startup scripts can take minutes to provision" and "time spent installing dependencies is time not spent on the task" ([template optimization](https://coder.com/docs/ai-coder/agents/platform-controls/template-optimization)). They also offer [prebuilt workspaces](https://coder.com/docs/admin/templates/extending-templates/prebuilt-workspaces), a warm pool, which devcrate does not need.
- **Gitpod (Ona), AWS runner:** development environments launch from Ona's own AMI, `gitpod/images/gitpod-next/ec2-runner-ami-*`, owner account `995913728426`. Customers are told to allowlist by owner ID "This ensures automatic access to new versions and security patches" ([AWS access requirements](https://ona.com/docs/ona/runners/aws/detailed-access-requirements)). Separately, built devcontainer images are cached in ECR, keyed on a hash of `devcontainer.json` and the Dockerfile ([devcontainer image cache](https://ona.com/docs/ona/runners/aws/devcontainer-image-cache)). That is the open "devcontainer image caching" item on the map, not a base image.

## 4. AWS equivalent: AMIs

- **Build:** `aws ec2 create-image --instance-id <i> --name <n>` from a customized instance. By default EC2 reboots the instance for consistency, and `--no-reboot` skips that. Or use `register-image` from an existing snapshot ([create an EBS-backed AMI](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/creating-an-ami-ebs.html)). Packer's `amazon-ebs` builder is the counterpart to the DigitalOcean builder.
- **Cost:** "Amazon EC2 creates snapshots of your instance's root volume … You're charged for the snapshots until you deregister the AMI and delete the snapshots." AWS's pricing page uses "$0.05 per GB-month for EBS Snapshots Standard storage" in its worked example ([EBS pricing](https://aws.amazon.com/ebs/pricing/)). The rate varies by Region.
- **Region scope:** "AMIs are Region-specific resources … to use the same AMI in multiple Regions, you must copy it from the source Region to each target Region" with `aws ec2 copy-image --source-image-id <ami> --source-region <src> --name <n> --region <dst>`. Copying is free unless you ask for a time-based copy, but "Standard storage and data transfer rates apply" and each copy is a new set of snapshots that is billed separately ([copy an AMI](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/CopyingAMIs.html)). That differs from DigitalOcean, where extra regions are free.
- **Referenced at launch:** as `ImageId` (DevPod's AWS provider passes `DiskImage` into `ImageId`, `pkg/aws/aws.go`).
- **Build time:** "it can take several minutes for the AMI-creation process to complete (sometimes up to 24 hours)".

**Fit with the provider contract:** on both clouds a base image is a per-region, provider-owned ID passed at create time, built by the same Packer workflow with a different builder. The contract can treat it as an opaque provider option (an `image` value in provider config) plus a provider-side "is this image present in this region" check. The image itself never has to cross the contract.

## 5. Recommendation for devcrate

1. Keep a fallback that works with no image: stock `ubuntu-24-04-x64` plus the install steps in user-data/setup. The first `up` in a new account or region must not depend on a prebuilt image.
2. Offer a base image as a speed-up: a droplet snapshot containing Docker and the devcontainer CLI (standalone installer, which bundles Node), built with a checked-in Packer template on `s-1vcpu-1gb` with no volume attached. Store its numeric ID in provider config, check `doctl compute image get <id>` for `regions` and `min_disk_size` before `create`, and fall back to the stock image if the region is missing.
3. Do not use uploaded custom images on DigitalOcean.
4. Do not depend on the Docker 1-Click slug. Its name and contents are not pinned, and it lacks Node and the CLI.
5. If Docker's `data-root` is moved onto the volume, as DevPod does (#3), container images and layers survive a stop either way, and the base image only saves the engine and CLI install.
6. Measure before promising anything (section 2).

## Open items

- Measure droplet create time (stock vs snapshot) and the Docker + devcontainer CLI install time on a fresh droplet.
- Confirm the live Docker 1-Click slug with `doctl compute image list-application`.
- Decide who builds and refreshes the image (`devcrate image build`, or a documented Packer template) and how stale images are detected.
