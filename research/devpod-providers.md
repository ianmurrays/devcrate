# How DevPod defines providers and agents

Research for [#2](https://github.com/ianmurrays/devcrate/issues/2) (child of map [#1](https://github.com/ianmurrays/devcrate/issues/1)).

## Sources

All claims below are from primary sources, read at these commits:

- `loft-sh/devpod` at `5a0efcbff6610ab114b421f68a890739a452e66b` (2025-11-14). Abbreviated `devpod:` below. Docs live in `docs/pages/` of the same repo.
- `loft-sh/devpod-provider-digitalocean` at `cca4f90f46507f27a690db027bc7f611d8249151` (2026-01-05). Abbreviated `do:` below.
- `loft-sh/devpod-provider-ssh` (main, shallow clone) for comparison.

Maintenance status as observed via `gh`: `loft-sh/devpod` is not archived, last push 2025-11-14, latest release `v0.7.0-alpha.34` (pre-release, 2025-06-23).

## Short answer

A DevPod provider is a `provider.yaml` manifest: a set of user-configurable **options** (passed as env vars), a set of shell-script **exec verbs** (`init`, `command`, `create`, `delete`, `start`, `stop`, `status`), downloadable helper **binaries** (per OS/arch URL + sha256), and an **agent** section configuring the on-machine side. Real providers point every verb at their own Go binary (`${DO_PROVIDER} create`, etc.). The agent is the full `devpod` CLI itself, streamed onto the machine through the provider's `command` verb, which DevPod uses as an opaque stdin/stdout pipe; everything afterwards (SSH server, credential forwarding, port forwarding) is a gRPC tunnel over that pipe. Auth is just an option: the DO provider takes a raw API token (read from `DIGITALOCEAN_TOKEN`/`DIGITALOCEAN_ACCESS_TOKEN`), calls the API via `godo` (not `doctl`), and that same token is copied to the VM so the on-VM idle daemon can delete the droplet.

## 1. The `provider.yaml` schema

Top-level fields (`devpod: pkg/provider/provider.go:11`, `ProviderConfig`): `name`, `version`, `icon`, `iconDark`, `home`, `source`, `description`, `optionGroups`, `options`, `agent`, `exec`, `binaries`.

### Exec verbs

`ProviderCommands` (`devpod: pkg/provider/provider.go:253`) has `init`, `command`, `create`, `delete`, `start`, `stop`, `status`, plus `proxy` and `daemon` (used for DevPod Pro / platform modes). Semantics from `devpod: docs/pages/developing-providers/quickstart.mdx`:

- **command** (the only required verb): "how to run a command in the environment. DevPod will use this command to inject itself into the environment and route all communication through the commands standard output and input." DevPod supplies the command in `${COMMAND}`.
- **init**: validate options / check the provider is usable. Called when options change.
- **create**: if present, the provider is a *machine provider* and `delete` is also expected.
- **delete**, **start**, **stop**: machine lifecycle.
- **status**: prints one of `Running`, `Busy`, `Stopped`, `NotFound` on stdout.

Verbs are POSIX shell scripts; on Windows they run in an emulated shell (`mvdan/sh`), so non-trivial logic is pushed into a helper binary.

Non-machine providers (local Docker, Kubernetes, plain SSH) only define `command` (and optionally `init`). The SSH provider's exec is just `init: ${SSH_PROVIDER} init` and `command: ${SSH_PROVIDER} command` (`devpod-provider-ssh: hack/provider/provider.yaml`).

### Options

From `devpod: docs/pages/developing-providers/options.mdx`:

- Options are validated when the provider is added and passed to every verb **as environment variables**. They are also passed to the agent and usable in `agent.*`.
- Attributes: `description`, `default` (may reference `${OTHER_OPTION}`), `required` (CLI prompts if missing), `password` (masked in listings), `suggestions` (desktop app only), `command` (shell command whose stdout becomes the value), `local` (per machine/workspace), `global`, `cache` (re-run `command` after a duration, for expiring tokens), `hidden`.
- Built-ins include `DEVPOD` (path to the CLI), `DEVPOD_OS`, `DEVPOD_ARCH`, `MACHINE_ID`, `MACHINE_FOLDER` (local dir for per-machine state, e.g. SSH keys), `WORKSPACE_ID`, `PROVIDER_FOLDER`, etc.
- `optionGroups` only affects the desktop app.

### Binaries

From `devpod: docs/pages/developing-providers/binaries.mdx`: `binaries.NAME` is a list of `{os, arch, path (URL or local), checksum (sha256), archivePath}`. DevPod downloads the matching one when the provider is added, verifies the checksum, and exposes its path as `${NAME}` in verbs and option commands. `agent.binaries` has the same shape and is downloaded on the remote machine for use in `agent.exec.shutdown`.

### Agent section

`ProviderAgentConfig` (`devpod: pkg/provider/provider.go:77`): `local`, `path` (where to put the agent binary on the machine), `dataPath`, `downloadURL`, `inactivityTimeout` (machine), `containerInactivityTimeout` (container), `injectGitCredentials`, `injectDockerCredentials`, `exec.shutdown`, `binaries`, `dockerless`, `driver` (`docker` default, `kubernetes`, `custom`) and per-driver config.

## 2. The DigitalOcean provider, concretely

Manifest: `do: hack/provider/provider.yaml`.

- Options: `TOKEN` (required, `password: true`, `command:` echoes `$DIGITALOCEAN_TOKEN` or `$DIGITALOCEAN_ACCESS_TOKEN` from the laptop env, line 20), `REGION` (default `fra1`), `DISK_SIZE` (30), `DISK_IMAGE` (default `docker-20-04`), `MACHINE_TYPE` (default `s-4vcpu-8gb`), `INACTIVITY_TIMEOUT` (10m), `INJECT_GIT_CREDENTIALS`, `INJECT_DOCKER_CREDENTIALS`, `AGENT_PATH`, `AGENT_DATA_PATH`.
- Every verb is `${DO_PROVIDER} <verb>` (line 142 onwards). The same Go binary is listed under `binaries` (laptop: linux/darwin/windows) and under `agent.binaries` (VM: linux amd64/arm64), with `agent.exec.shutdown: ${DO_PROVIDER} stop` (line 118).
- The binary reads options from env (`do: pkg/options/options.go`, `FromEnv`), prefixes the machine ID with `devpod-`, and talks to the DO API with `godo.NewFromToken(token)` (`do: pkg/digitalocean/digitalocean.go`). It does **not** use `doctl`.
- It imports `github.com/loft-sh/devpod` as a Go library (pinned to pseudo-version `v0.0.3-0.20230512100016-aee23bbc9aad` in `do: go.mod`) for SSH helpers, logging and the `client.Status` enum. So "external executable" still meant linking against DevPod's core.

Lifecycle (`do: pkg/digitalocean/digitalocean.go`, `do: cmd/*.go`):

- **SSH identity**: DevPod generates a per-machine 2048-bit RSA keypair (`id_devpod_rsa`) in `MACHINE_FOLDER` (`devpod: pkg/ssh/keys.go`). `create`/`start` embed the public key in cloud-init.
- **create / start**: both call `buildInstance` (`do: cmd/create.go:107`) then `Create`: find-or-create a block-storage volume named after the machine (ext4, tag `devpod`), then create a droplet (tag `devpod`) with that volume attached and `UserData` from `GetInjectKeypairScript` (`do: cmd/create.go:53`). That cloud-init script mounts the volume at `/home/devpod`, moves Docker's `data-root` onto it (line 74), creates user `devpod` with `NOPASSWD` sudo (line 86), and installs the public key in `authorized_keys`.
- **stop**: deletes the droplet, keeps the volume, polls until `status` says `Stopped`. (DevPod's own docs: "Deletes the whole machine on inactivity as stopped machines are still billed by DigitalOcean", `devpod: docs/pages/developing-providers/agent.mdx`.)
- **status**: droplet exists and `active` → `Running`; exists but not active → `Busy`; no droplet but volume exists → `Stopped`; neither → `NotFound`. Output is the bare string on stdout.
- **delete**: detach and delete the volume, then delete the droplet.
- **command**: look up the droplet's public IPv4, open an SSH client as `devpod` with the machine's private key, and run `$COMMAND` with the process's stdin/stdout/stderr wired through (`do: cmd/command.go`).
- **init**: list droplets to check the token works.

Resources are found by name (`ListByName`, `ListVolumes{Name}`), erroring if more than one matches. No local state beyond the SSH key.

## 3. How the agent gets onto the machine and runs

The agent is not a separate program: it is the `devpod` binary itself, run as `devpod agent ...` (`devpod: docs/pages/developing-providers/agent.mdx`; commands in `devpod: cmd/agent/`).

Injection (`devpod: pkg/agent/inject.go`, `devpod: pkg/inject/inject.sh`):

1. DevPod runs a templated shell script through the provider's `command` verb.
2. The script prints `ping` and waits for `pong` on stdin (handshake so it does nothing unless DevPod is on the other end).
3. It checks whether `<agent path> version` equals the local DevPod version; if so it skips installation.
4. Otherwise it finds a writable install dir (escalating with `sudo -n` or `su` if needed, failing if sudo needs a password), checks the mount is not `noexec`, then either receives the binary **streamed over stdin** from the laptop (`inject`) or downloads it with curl/wget from GitHub releases (`download`), whichever is preferred, falling back to the other. It reports `ARM-true|false` so the laptop sends the right arch.
5. It exports `DEVPOD_AGENT_URL` and execs the real command.
6. The laptop retries the whole thing every 3 s for up to 5 minutes (`waitForInstanceConnectionTimeout`) while the VM boots.

The binary sent from the laptop is the running executable if the laptop is linux/same-arch, otherwise a cached or freshly downloaded `devpod-linux-<arch>` in `$TMPDIR/devpod-cache`.

Running the workspace (`devpod: cmd/up.go`, `devPodUpMachine`):

- After `startWait` (status/start loop), DevPod serialises the workspace info (`AgentWorkspaceInfo`) into a compressed string and runs `'<agent>' agent workspace up --workspace-info '<blob>'` via the inject path.
- In parallel it runs `'<agent>' helper ssh-server --stdio` over the same `command` channel and establishes a gRPC `Tunnel` service over stdio (`devpod: pkg/agent/tunnel/tunnel.proto`). That service has 14 RPCs: `Ping`, `Log`, `SendResult`, `DockerCredentials`, `GitCredentials`, `GitSSHSignature`, `GitUser`, `LoftConfig`, `GPGPublicKeys`, `KubeConfig`, `ForwardPort`, `StopForwardPort`, `StreamWorkspace`, `StreamMount`. Credentials are served from the laptop on demand (e.g. `devpod agent git-credentials` is a git credential helper that calls back through the tunnel), rather than being copied.
- On the VM the agent writes the workspace info to `<dataPath>/contexts/<ctx>/workspaces/<id>/workspace.json` (mode 0600, `devpod: pkg/agent/agent.go:237`), builds and starts the devcontainer with Docker, and installs a system service (`devpod agent daemon`, via `takama/daemon`, `devpod: pkg/daemon/agent/daemon.go:80`, called from `devpod: cmd/agent/workspace/up.go:399`).

### Idle shutdown

Two levels (`devpod: docs/pages/developing-providers/agent.mdx`):

- **Machine** (`inactivityTimeout`): `devpod agent daemon` (`devpod: cmd/agent/daemon.go`) wakes every 60 s, globs all `workspace.json` files, takes the newest **mtime** as the last activity (adding 20 min if a workspace "busy" file exists, i.e. a build is running), and if it is older than the timeout (default 20 min, `devpod: pkg/agent/agent.go:25`) runs `agent.exec.shutdown` with the provider options and agent binaries as env (`custom.ToEnvironWithBinaries`, `devpod: pkg/driver/custom/custom.go:307`). Workspaces with no `exec.shutdown` are ignored.
- What bumps that mtime: `workspace.json` is rewritten whenever a `devpod agent` subcommand decodes workspace info with write enabled, i.e. `container-tunnel` (run per `devpod ssh` / IDE connection, `devpod: pkg/tunnel/container.go:218`), `workspace up`, `workspace build`, `workspace stop`, `workspace update-config`. I found no periodic refresh during a long-lived connection, so activity appears to mean "a new connection or command started", not "traffic is flowing". Not verified at runtime.
- **Container** (`containerInactivityTimeout`): `helper ssh-server --track-activity` touches `/tmp/devpod.activity` every 10 s while a session is open (`devpod: cmd/helper/ssh_server.go:102`), and an in-container daemon kills PID 1 when that file is older than the timeout (`devpod: cmd/agent/container/daemon.go`). This is for non-machine providers.

## 4. Provider auth

DevPod has no auth mechanism of its own: credentials are just options. Patterns in the official providers (`devpod: docs/pages/developing-providers/agent.mdx`, `options.mdx`):

- **DigitalOcean**: long-lived API token as a `password` option, defaulted from env vars. That token is part of the filled options, which are carried in `AgentWorkspaceInfo.Options` (`devpod: pkg/provider/workspace.go:185`), written to `workspace.json` on the VM, and put in the env of `${DO_PROVIDER} stop` so the VM can delete its own droplet. In other words, **the full account token lives on the VM**.
- **AWS / GCloud**: the local `aws`/`gcloud` CLI mints a short-lived token via a `hidden`, `local`, `cache: 5m` option with a `command:` that runs the provider binary; that token is what reaches the agent for shutdown.
- **Azure**: `shutdown -t now` (no API credential needed, because a stopped Azure VM is not billed for compute).

## 5. What made it heavy

From what the code and docs show:

- **The agent is the whole DevPod CLI** (devcontainer builder, SSH server, credential helpers, daemons, Pro/platform code), injected and version-matched on every connect.
- **`command` as a universal transport.** Every interaction, including the SSH server and a 14-RPC gRPC tunnel, is multiplexed over the stdin/stdout of one provider verb. It lets one contract cover Docker, Kubernetes, SSH and VMs, but each provider must implement a faithful stdio pipe, and the injection protocol (ping/pong, arch probe, stream-or-download, sudo escalation, retries) has to live in core.
- **Provider binary on both sides.** Each provider ships signed per-OS/arch binaries for the laptop *and* the VM, so a release means 5+ artifacts and checksums templated into the manifest.
- **Providers link DevPod core** as a Go module, coupling provider releases to DevPod internals.
- **Generality**: drivers (docker/kubernetes/custom), dockerless builds, machine vs non-machine providers, machine sharing, Pro proxy/daemon verbs, Windows emulated shell, desktop-app-only manifest fields (icons, `optionGroups`, `suggestions`).
- **Credential proxying** for git, git SSH signing, GPG, docker, kube and Loft config, all through the tunnel.

## 6. Recommendation for devcrate

Copy:

- The **verb set** for machine lifecycle (`init`, `create`, `start`, `stop`, `status`, `delete`) and the **status enum** (`Running`/`Busy`/`Stopped`/`NotFound`). It is small and was sufficient for every machine provider.
- **Options as env vars** with a declared list (name, description, default, required, secret). Skip `suggestions`, `optionGroups`, icons, `cache` until needed.
- The DO **"stop = delete droplet, keep a named volume"** pattern, with resources looked up by name/tag so the provider stays stateless. Note that block storage volumes still cost money while they exist (not quantified here).
- **Per-machine SSH keypair generated locally and injected via cloud-init** user data.

Drop or simplify:

- **Drop `command` as a transport.** devcrate's IDE path is an SSH config entry, so core can SSH directly. Have `create`/`start`/`status` return JSON (e.g. `{"state": "...", "ip": "..."}`) and let core own the SSH connection.
- **Drop the gRPC tunnel.** Use plain SSH agent forwarding for git and SSH `-R`/`-L` for anything else, which matches the map's MVP credential scope.
- **Make the on-VM agent a small purpose-built binary** (idle detection + self-stop), installed by cloud-init or `scp` rather than an in-band injection protocol.
- **Don't require a provider binary on the VM.** The VM needs exactly one outbound capability: destroy its own droplet. That can be a direct DO API call from the devcrate agent.
- **Base idle on real activity** (e.g. live SSH sessions / recent traffic), not "last connection started", given the mtime behaviour above.

Open question for the provider-contract / auth ticket ([#6](https://github.com/ianmurrays/devcrate/issues/6)): shelling out to `doctl` on the laptop covers create/start/status/delete with the user's existing auth, but because idle detection runs on the VM and "stop" must destroy the droplet, *some* DigitalOcean credential has to be on the VM. DevPod's answer was to copy the full account token. Whether DigitalOcean offers a narrower or short-lived credential suitable for "delete this one droplet" was not researched here.
