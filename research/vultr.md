# Vultr against the provider contract

Research for [#21](https://github.com/ianmurrays/devcrate/issues/21), child of map [#1](https://github.com/ianmurrays/devcrate/issues/1). Researched 2026-10-14 from Vultr docs (docs.vultr.com, read as the Markdown each page serves at `<url>.md`), vultr.com pages (metadata, pricing, block storage product page, read in a browser because they sit behind a Cloudflare challenge), the public unauthenticated `GET https://api.vultr.com/v2/plans` and `/v2/regions` endpoints, the `vultr/vultr-cli` source @ `e996e09` (v3.11.0, 2026-08-07), `vultr/govultr` @ `7501091`, Vultr's API spec as vendored in `vultr/vultr-python-client` @ `8cd1eaf` (`openapi-client/swagger.json`, 2025-08-13), the IAM spec `vultr/iam-management-api` @ `179c6d4` (2026-06-01), and cloud-init's Vultr datasource (`canonical/cloud-init` main). No Vultr resources were created; nothing here was measured.

The www.vultr.com/api reference page could not be read (Cloudflare challenge). Where this file cites "the API spec", it means the 2025-08-13 `swagger.json` above, which predates VX1 and IAM. Newer behaviour is cited from docs.vultr.com.

DigitalOcean figures for comparison come from the resolution on [#3](https://github.com/ianmurrays/devcrate/issues/3) and `research/do-stop-resume.md` on branch `research/do-stop-resume`.

## Answer

- **Yes, Vultr can meet the provider contract the same way DigitalOcean does.** A stopped instance is billed in full, so Stop = destroy the instance and keep state on a Block Storage volume. Resume = create a new instance and attach the volume.
- **API key scoping is better than DigitalOcean's, on paper.** Vultr IAM (new in 2026) lets a policy grant `compute.instance.Delete` on one resource, written `instance:<id>`. A service user's API key can therefore be limited to deleting one instance, given an expiry date, and IP-allowlisted. DigitalOcean's `droplet:delete` scope covers every droplet in the team. There is also an instance identity token (a JWT from the metadata service) that can assume an IAM role with no stored key on the VM. But nothing documented ties that role to the calling instance only, so any instance in the account could assume it.
- **Two things are unverified and must be tested before relying on them:** that `compute.instance.Delete` alone authorizes `DELETE /v2/instances/{id}`, and that IAM is available on every account (the docs assume "an existing organization").
- **Gaps compared with DigitalOcean:** `vultr-cli` has no IAM, API-key or allowlist commands, and `instance list` has no `--tag` filter. Attaching a volume at instance create is documented only for VX1 plans. That an ordinary attached volume survives instance destroy is not documented. Billing is hourly (capped at 672 hours/month on non-VX1 plans), not per second.
- **Lead worth keeping:** VX1 plans can boot from a Block Storage volume that survives instance deletion (documented), so a VX1 Environment would keep its whole root disk across a Stop. The smallest VX1 plan is 2 vCPU / 8 GB at $43.80/mo, available in 9 regions.

## 1. Stopped instances are billed: Stop must mean destroy

- Cloud Compute FAQ: "Does a Vultr Cloud Compute instance incur any charges when stopped? Yes, a stopped instance incurs normal charges as it would while running. However, the instance does not incur any charges when destroyed." ([Cloud Compute FAQ](https://docs.vultr.com/products/compute/instances/cloud-compute/faq))
- Block Storage product page FAQ: "instances in a stopped state continue to reserve dedicated system resources (RAM, SSD storage, IP aliases, vCPU) and therefore incur charges until you destroy the instance." ([Block Storage](https://www.vultr.com/products/block-storage/))
- Destroy: `vultr-cli instance delete <id>` (alias `destroy`) or `DELETE https://api.vultr.com/v2/instances/{instance-id}` ([destroy how-to](https://docs.vultr.com/products/compute/instances/cloud-compute/management/destroy-instance); `cmd/instance/instance.go`).

**Billing increment.** Plan prices are given per hour and per month. The pricing page says "VX1 instances are billed on actual hours used each month and are not capped at 672 hours", which implies other plans are capped at 672 hours a month. The pricing page script also sets `hours_in_month = 672` ([pricing](https://www.vultr.com/pricing/)). I found no statement of partial-hour rounding or a minimum charge. DigitalOcean bills per second with a 60 s minimum. If Vultr rounds up to whole hours, frequent stop/resume cycles cost more than on DigitalOcean. That is not documented, so measure it on an invoice.

## 2. Block Storage

| | NVMe (`high_perf`) | HDD (`storage_opt`) | Source |
|---|---|---|---|
| Price | $0.10/GB-month ("$1 per 10 GB") | $0.025/GB-month | NVMe: product page copy and pricing page script `block_per_gb_month = 0.1`. HDD: only in the product page's calculator script (`hdd: { ..., pricePerUnit: 1 / 40 }`); the displayed copy did not switch to HDD, so treat this figure as lower-confidence. |
| Size | 10 to 10,000 GB per the API spec; the FAQ says the minimum is 1 GB | 40 to 40,000 GB | `swagger.json` `POST /blocks` `size_gb`/`block_type`; [Block Storage FAQ](https://docs.vultr.com/products/storage/block-storage/faq) |
| Performance | 10,000 IOPS, 400 MiB/s, 50% burst for 60 s | 500 IOPS, 100 MiB/s | product page FAQ |
| Regions | 12 of 33 regions: ams, atl, blr, ewr, lax, lhr, mxp, nrt, ord, sea, sgp, syd | 31 of 33 | `GET /v2/regions` `options` (`block_storage_high_perf`, `block_storage_storage_opt`), fetched 2026-10-14 |

- **Region rule:** a volume attaches only to an instance in the same location ([FAQ](https://docs.vultr.com/products/storage/block-storage/faq)). One instance at a time, up to 16 volumes per instance.
- **Create:** `vultr-cli block-storage create --region ewr --size 50 --block-type high_perf --label devcrate-<name>` or `POST /v2/blocks {"region","size_gb","label","block_type"}` ([provisioning](https://docs.vultr.com/products/storage/block-storage/provisioning)).
- **Attach after create (documented for all plans):** `vultr-cli block-storage attach <block-id> --instance <instance-id> --live` or `POST /v2/blocks/{block-id}/attach {"instance_id": "...", "live": true}`. `live: false` restarts the instance to attach it (`swagger.json`; [attach how-to](https://docs.vultr.com/products/storage/block-storage/management/attach-instances)). So the Provider's `up` must create the instance, wait until it is active, attach the volume live, and only then can the VM mount it. That is one call more than DigitalOcean's `--volumes` at create. The VM's user_data has to wait for the device to appear.
- **Attach at create:** `POST /v2/instances` accepts `block_devices: [{block_id, bootable, disk_size, label}]` (govultr `InstanceBlockDevice`; `vultr-cli instance create --block-devices`, whose help says "At least one block is required for VX1 plans"). Vultr documents it only for VX1 ([VX1 provisioning](https://docs.vultr.com/products/compute/instances/vx1-cloud-compute/provisioning)). It is absent from the 2025-08 API spec. Whether it works on `vc2`/`vhp` plans is undocumented.
- **Device path:** "The first Vultr Block storage volume attaches to Linux as `/dev/vdb` and additional volume disk names increment in alphabetical order" ([mount on Linux](https://docs.vultr.com/products/storage/block-storage/mount/linux)). The guide mounts by filesystem UUID in fstab with `nofail`. devcrate should do the same, or use a filesystem label, rather than hard-coding `/dev/vdb`.
- **Survives instance destroy:**
  - VX1 bootable volume: documented. "Deleting an Vultr VX1™ instance that is booted from a Vultr Block Storage (VBS) bootable volume does not delete the underlying VBS volume" ([VX1 delete](https://docs.vultr.com/products/compute/instances/vx1-cloud-compute/management/delete)).
  - An ordinary data volume on a Cloud Compute instance: **not documented.** The destroy how-to says nothing about attached volumes. Volumes are separate billable resources with their own delete endpoint, and the FAQ describes moving them between instances, so survival is the likely behaviour. Test it before the spec depends on it. DigitalOcean was in the same position, except DevPod's provider showed it working in practice. I found no equivalent Vultr provider to point to.
- **Detached-volume billing:** not stated anywhere I read. DigitalOcean says explicitly that volumes are billed attached or not.

## 3. Create with user data and tags; list by tag

- **Create:** `vultr-cli instance create --region ewr --plan vc2-2c-4gb --os <os-id> --label devcrate-<name> --host devcrate-<name> --ssh-keys <key-id> --tags devcrate:<name> --userdata-file setup.sh -o json` (`cmd/instance/instance.go`; `--os`/`--snapshot`/`--image`/`--app`/`--iso` are mutually exclusive and one is required). vultr-cli base64-encodes the user data itself. Over the raw API, `user_data` must already be base64-encoded (`swagger.json` `POST /instances`).
- **Tags:** `tags: []string` on create (`tag` singular is deprecated), `PATCH /v2/instances/{id} {"tags": [...]}` or `vultr-cli instance tags <id> --tags a,b` to change them ([tags how-to](https://docs.vultr.com/products/compute/instances/cloud-compute/management/tags)). I found no documented tag-format rules, so test whether a colon (`devcrate:<name>`) is accepted.
- **List by tag:** the API supports `GET /v2/instances?tag=<tag>` (also `label`, `region`, `main_ip`) (`swagger.json`; govultr `ListOptions`). **`vultr-cli instance list` has no tag flag**, only `--cursor` and `--per-page` (max 500). For the contract's `list` verb the Provider either curls `?tag=` or runs `vultr-cli instance list -o json --per-page 500` and filters with `jq`. A prefix match such as `devcrate:*` must be done client-side in both cases, because `?tag=` is documented as "Filter by specific tag".
- **User data afterwards:** `vultr-cli instance user-data get|set` and `PATCH /v2/instances/{id} {"user_data": ...}` exist. A user-data script "runs as root and only on the first boot" ([cloud-init guide](https://docs.vultr.com/how-to-deploy-a-vultr-server-with-cloudinit-userdata)). That is fine, because every resume is a new instance.

## 4. Metadata service

- Endpoint `http://169.254.169.254/v1/`. "Function calls are performed over standard HTTP with no authentication." "The API functions described in this document will only return information about the calling instance." ([Metadata API](https://www.vultr.com/metadata/))
- **Instance ID without auth:** `curl http://169.254.169.254/v1/instance-v2-id` returns the UUID used by the v2 API (`/v1/instanceid` is the legacy "SUBID"). Also `/v1.json` for the whole tree.
- **User data is readable by any process.** The metadata page documents no user-data path. cloud-init's Vultr datasource fetches `http://169.254.169.254/v1.json` with the header `Metadata-Token: cloudinit` and reads `user-data` and `vendor-data` from it (`cloudinit/sources/helpers/vultr.py` `read_metadata`; `DataSourceVultr.py`). That header is a constant string in public source code, not a secret. Any local process can send it. Conclusion as on DigitalOcean: never put a credential in user data.
- **Identity token (new, 2026-09):** `curl http://169.254.169.254/v1/identity/token` returns a JWT with `sub: instance:<uuid>`, `acctid`, `orgid`, `region`, valid for 5 minutes ([obtain identity token](https://docs.vultr.com/platform/iam/roles/instance-and-bare-metal-auth/obtain-identity-token)). No authentication is described, so any process on the VM can get one. See section 5B.

## 5. API key scoping (Self-stop)

### What exists

- **Legacy subuser ACLs:** `vultr-cli user create --acl` takes `manage_users subscriptions_view subscriptions billing support provisioning dns abuse upgrade firewall alerts objstore loadbalancer` (`cmd/users/user.go`). None of these is delete-only or per-instance. Not useful here.
- **IAM policies (2026):** policy documents with `Action`, `Effect` and `Resource`. Instance actions are `compute.instance.Create|Delete|List|Read|Update` ([compute actions](https://docs.vultr.com/platform/iam/policies/iam-policy-actions-map/compute-actions)). "To restrict permissions to specific resources instead of all resources, use the `type:id` format in the `Resource` field ... (such as `instance`, ...)" ([create a policy](https://docs.vultr.com/platform/iam/policies/how-to-create-a-policy)). Policies attach to users, groups or roles.
- **Service users:** "API-only accounts designed for programmatic access", created in the Console or Terraform ([service user](https://docs.vultr.com/platform/iam/service-user/how-to-create-a-service-user)). Over the API, govultr's `UserReq` has a `service_user` field on `POST /v2/users`, but the doc doesn't show an API path.
- **Multiple named keys per user, with expiry:** `POST /v2/users/{user-id}/apikeys {"name": "...", "expire": true, "date_expire": "2030-01-01T00:00:00Z"}`. The key is returned once ([create an API key for a user](https://docs.vultr.com/platform/iam/organizations/manage-user-api-access/how-to-create-an-api-key-for-a-user)). Unlike DigitalOcean, keys **can be created through the API**, so the Provider can mint a fresh key on every `up`.
- **Per-user IP allowlist:** `POST /v2/users/{user-id}/ip-whitelist {"subnet": "...", "subnet_size": 32}`, plus `GET` and `DELETE` on the same path. The default is allow-all ([API access control](https://docs.vultr.com/platform/iam/organizations/manage-user-api-access/how-to-manage-vultr-api-access-control-for-users)).
- **Assumable roles and sessions:** `POST /v2/assumed-roles/assume` returns a time-bound `session_token` ([assume a role](https://docs.vultr.com/platform/iam/roles/assumed-roles/how-to-assume-a-role)).
- **vultr-cli has none of the IAM, API-key or allowlist commands.** The command list has no organization, policy, role or key subcommands. govultr has the policy, role and trust calls (`organization.go`) but no `/apikeys` or `/ip-whitelist` calls. All of this is raw `curl` from the Provider.

### A. Per-environment scoped key (recommended)

One-time setup: a service user, e.g. `devcrate-selfstop`, holding no policies except those below.

On each `up`, after the instance exists, the Provider calls the API with the user's admin-level key:

1. `POST /v2/policies` with `{"name": "devcrate-<name>-selfstop", "policy_document": {"Version": "2026-03-20", "Statement": [{"Action": ["compute.instance.Delete"], "Effect": "Allow", "Resource": ["instance:<new-id>"]}]}}`. Or `PUT /v2/policies/{id}` to repoint an existing per-environment policy at the new ID.
2. `POST /v2/policies/{policy-id}/users/{user-id}` to attach it ([attach policy to user](https://docs.vultr.com/platform/iam/policies/how-to-attach-a-policy-to-a-user)).
3. Optionally, `POST /v2/users/{user-id}/ip-whitelist` with the VM's main IP /32.
4. `POST /v2/users/{user-id}/apikeys` with a short `date_expire`, then push the key over SSH into a root-only file, as on DigitalOcean.

On Stop or Delete, the Provider removes that environment's policy (and allowlist entry). The `self-stop` script (my composition from the documented endpoints):

```sh
ID=$(curl -sf http://169.254.169.254/v1/instance-v2-id)
curl -sf -X DELETE -H "Authorization: Bearer $(cat /var/lib/devcrate-secrets/token)" \
  "https://api.vultr.com/v2/instances/$ID"
```

**Blast radius if the key leaks:** it can delete only the instances named in the policies attached to that service user, only before it expires, and only from allowlisted IPs. The policies cover every running devcrate Environment if all of them share one user. One service user per Environment narrows it to the one instance, at the cost of a user per Environment. On DigitalOcean the same leak can delete every droplet in the team.

**Unverified:** I did not confirm that `compute.instance.Delete` on `instance:<id>` alone is enough for `DELETE /v2/instances/{id}`. The IAM spec has `GET /v2/endpoint-action-maps`, which maps each endpoint to its `required_action` and `resource_pattern`. It needs a key, so this is the first thing to check. Whether IAM is enabled on every account is also not stated. The docs assume "an existing organization".

### B. Instance identity token, no stored key (lead)

A role trust with `"trust_type": "TemporaryAssumption", "trusted_oidc_issuer_source": "instance"` lets an instance trade its identity JWT for a role session ([configure role trust](https://docs.vultr.com/platform/iam/roles/instance-and-bare-metal-auth/configure-role-trust), [assume with identity token](https://docs.vultr.com/platform/iam/roles/instance-and-bare-metal-auth/assume-role)). Nothing to push, nothing to expire.

- **Limit:** "Once the trust exists, any instance or bare metal server on your account can assume the role." The documented trust conditions are account ID (pinned automatically), time of day and source IP. Nothing shows pinning the JWT's `sub` to one instance, and nothing shows a policy variable such as "the calling instance". So the role's `Resource` list must name the devcrate instances explicitly, and any instance in the account (including one compromised through a devcontainer on another Environment) can delete any of them.
- **Not documented:** the exact request body for the instance case. The assume-role doc says to omit `user_id` and send the JWT as the Bearer token, and that `auth_method` is one of `api_key`, `jwt`, `oidc`, but gives no instance example. These pages were published 2026-09-21 and updated 2026-10-07, so they are very new.

## 6. Base images: snapshots

- Price: "$0.05/GB per month for the compressed size of your Snapshots" ([Snapshots FAQ](https://docs.vultr.com/products/storage/snapshots/faq); also on the [pricing page](https://www.vultr.com/pricing/)). DigitalOcean charges $0.06/GB-month.
- Region scope: "Vultr Snapshots span across all regions." A snapshot taken in London can deploy in New York. No transfer step is needed, unlike DigitalOcean's `image-action transfer`.
- Create from an instance: `vultr-cli snapshot create --id <instance-id>` or `POST /v2/snapshots {"instance_id"}`. Create from a URL: `vultr-cli snapshot create-url`. Deploy: `vultr-cli instance create --snapshot <id>` ([snapshots](https://docs.vultr.com/products/storage/snapshots/provisioning)).
- Packer: the official `vultr/packer-plugin-vultr` builder deploys, provisions, snapshots and destroys ([Packer guide](https://docs.vultr.com/creating-snapshots-with-packer)). Same workflow as the DigitalOcean base-image decision ([#13](https://github.com/ianmurrays/devcrate/issues/13)).
- Block Storage volumes cannot be included: "Vultr automated server backup does not back up any attached Vultr Block Storage volumes." Volume snapshot pages exist (`products/storage/block-storage/block-storage-snapshot/*`) but were not read for this ticket.

## 7. Container registry

Vultr Container Registry is OCI-compatible. Plans: Start Up free with 10 GB, Business $5/mo with 20 GB, Premium $10/mo with 50 GB, Enterprise $20/mo with 1 TB. Pushes stop when storage is full. Docker login uses a username plus API key, and registries can be made public. Creation takes "about 2 minutes" ([Container Registry FAQ](https://docs.vultr.com/products/orchestration/container-registry/faq)). `vultr-cli container-registry` exists (`cmd/containerregistry`). There are IAM actions for it ([container registry actions](https://docs.vultr.com/platform/iam/policies/iam-policy-actions-map/container-registry-actions)), not read in detail.

## 8. Price comparison: 2 vCPU / 4 GB

From `GET https://api.vultr.com/v2/plans` (public, fetched 2026-10-14):

| Plan | Type | Disk | Transfer | $/mo | $/hr | Regions |
|---|---|---|---|---|---|---|
| `vc2-2c-4gb` | Regular (shared, older Intel, SSD) | 80 GB | 3 TB | 20 | 0.027 | 30 |
| `vhp-2c-4gb-amd` / `-intel` | High Performance (NVMe) | 100 GB | 5 TB | 24 | 0.033 | 31 / 32 |
| `vhf-2c-4gb` | High Frequency | 128 GB | 3 TB | 24 | 0.033 | 31 |
| `voc-c-2c-4gb-50s-amd` | Optimized (dedicated CPU) | 50 GB | 5 TB | 40 | 0.055 | 34 |
| `vx1-g-2c-8g` (no 4 GB VX1 exists) | VX1 (dedicated, boots from Block Storage) | Block Storage | 5 TB | 43.80 | 0.06 | 9 |

"Regions" is the length of each plan's `locations` list. `/v2/regions` returns 33 regions, but the `voc` plan lists 34 locations, so plan lists can name locations that `/v2/regions` does not.

DigitalOcean Basic 2 vCPU / 4 GiB / 80 GiB is $24/mo ($0.03571/hr). Vultr `vc2` is $4 cheaper, and `vhp` costs the same.

| Storage | Vultr | DigitalOcean |
|---|---|---|
| Block volume | NVMe $0.10/GB-month (HDD $0.025, low confidence) | $0.10/GiB-month |
| Snapshot | $0.05/GB-month, all regions | $0.06/GB-month, per region (transfer is free) |
| 50 GB volume, stopped | $5.00/month NVMe | $5.00/month |

The NVMe volume regions (12) are a subset of the `vc2` regions except `mxp`, which `vc2-2c-4gb` doesn't list. Usable regions for the Stop/Resume design are therefore 11, unless HDD volumes are acceptable. At 500 IOPS, HDD volumes are a poor fit for a Docker data-root.

## 9. VX1: boot from Block Storage (lead)

VX1 can boot from a `high_perf` volume (`"bootable": true`, created up front with `os_id`, or created inline with `disk_size`). The volume survives instance deletion (section 2). Resume is `POST /v2/instances {"plan": "vx1-g-2c-8g", "block_devices": [{"block_id": "<uuid>", "bootable": true}], ...}`. The whole root disk, including packages, the Agent and Docker images, comes back. That removes the reinstall-on-every-resume cost that DigitalOcean's design pays ([#4](https://github.com/ianmurrays/devcrate/issues/4), [#13](https://github.com/ianmurrays/devcrate/issues/13)). Vultr advertises "instant provisioning in under 15 seconds" for VX1 ([pricing](https://www.vultr.com/pricing/)). That is a marketing claim, so measure it. Costs: there is no 4 GB VX1 plan, so the floor is $43.80/mo for 2 vCPU / 8 GB, billed on actual hours, in 9 regions (ewr, ord, sea, atl, ams, lhr, syd, nrt, mxp). The doc's example for an existing bootable volume still passes `os_id: 2284` alongside the `block_id`; whether `os_id` is required, or ignored, in that case is not stated.

## 10. vultr-cli maturity

- Official Vultr repo, created 2019-05-31, 562 stars, 23 open issues, not archived, last push 2026-09-09 (GitHub API).
- Releases: v3.11.0 (2026-08-07), v3.10.0 (2026-04-16), v3.9.2 (2026-03-26), v3.8.0 (2025-09-19), v3.7.0 (2025-09-03), v3.6.0 (2025-07-17). Built on `govultr`, last commit 2026-10-07.
- `-o json|yaml` output on every command (`cmd/root.go`). Auth is via `VULTR_API_KEY` or the config file.
- Gaps relevant to devcrate: no `--tag` filter on `instance list`; no IAM, API-key or allowlist commands; `--block-devices` takes a custom `key:value,.../...` string format. A Vultr Provider would be part vultr-cli and part curl. Note that `doctl` covers everything the DigitalOcean Provider needs.

## Proposed Provider mapping (sketch)

| Verb | Calls |
|---|---|
| `up` (first) | `block-storage create --block-type high_perf`; `instance create --tags devcrate:<name> --userdata-file ...`; poll `instance get` until active; `block-storage attach --live`; IAM policy, attach, allowlist and API key (curl); push the key over SSH |
| `up` (resume) | same, minus the volume create; repoint the policy to the new instance ID |
| `stop` | `instance delete`; remove the environment's policy and allowlist entry |
| `delete` | `stop`, then `block-storage delete` |
| `status` / `list` | `GET /v2/instances?tag=devcrate:<name>` or `instance list -o json` plus `jq` |
| `self-stop` | `curl -X DELETE /v2/instances/$(curl 169.254.169.254/v1/instance-v2-id)` with the pushed key |

## Open items to test before speccing a Vultr Provider

1. `compute.instance.Delete` on `instance:<id>` alone allows the delete (check `/v2/endpoint-action-maps`, then try it).
2. IAM and service users are available on a normal personal account.
3. An attached data volume survives `instance delete` on a `vc2`/`vhp` plan, and detached volumes keep billing.
4. Whether `block_devices` with an existing data `block_id` works at create on non-VX1 plans.
5. Hourly rounding and minimum charge, from a real invoice.
6. Whether tags accept `:`.
