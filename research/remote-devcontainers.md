# Running devcontainer.json on a remote VM

Research for [#4](https://github.com/ianmurrays/devcrate/issues/4), child of map [#1](https://github.com/ianmurrays/devcrate/issues/1).

**Question:** What does it take to run a devcontainer.json on a remote Linux VM: does `@devcontainers/cli` (up/exec, features, forwardPorts, postCreate hooks) work headless there, what does it need installed, and how do DevPod and Codespaces handle it? Which devcontainer.json fields matter for credentials and port forwarding?

## Answer

- `@devcontainers/cli` works headless on a Linux VM. It is a plain CLI with no IDE or display dependency, and it is the spec's reference implementation. On the VM it needs Docker (engine and CLI, plus the compose plugin for `dockerComposeFile` configs) and either Node >= 20 or the standalone installer, which bundles Node. `devcrate` should run it on the VM, not on the laptop.
- It covers build, Features, container create/start, all lifecycle hooks, `exec`, dotfiles and env injection.
- It does **not** forward ports. `forwardPorts` is parsed and merged, but no code acts on it. Only `appPort` becomes a Docker `-p` publish.
- It does **not** handle SSH-agent forwarding. Credential plumbing is left to the caller, through `--mount`, `--remote-env` and `--secrets-file`.
- `stop`/`down` are not implemented. Use `docker stop`/`docker rm` directly.
- `initializeCommand` and `${localEnv:...}` resolve wherever the CLI runs. If the CLI runs on the VM, that means the VM, not the laptop. This matches how Codespaces behaves.
- DevPod does not use `@devcontainers/cli` at all. It reimplements the spec in Go and runs that code as an agent on the VM. It also adds its own in-container helper binary and SSH server, which provide port forwarding and credential forwarding.
- Codespaces runs the dev container on a VM with these limits:
  - secrets arrive as env vars, but not at build time or inside Features;
  - bind mounts are ignored (except the Docker socket);
  - `forwardPorts` `"host:port"` entries are unsupported;
  - `${localEnv}` refers to the cloud host.

The design consequence: `devcrate` can delegate "make the container" to `@devcontainers/cli` on the VM. Port forwarding and credential forwarding (SSH agent, opt-in env/files) remain `devcrate`'s job, the same as in DevPod.

## Sources

- devcontainers/cli @ [`155f8b29`](https://github.com/devcontainers/cli/tree/155f8b29e1c521bf365e2aebd74f3258a9160718) (2026-10-02), v0.89.0
- loft-sh/devpod @ [`5a0efcbf`](https://github.com/loft-sh/devpod/tree/5a0efcbff6610ab114b421f68a890739a452e66b)
- Spec: [containers.dev JSON reference](https://containers.dev/implementors/json_reference/), [devContainer.base.schema.json](https://github.com/devcontainers/spec/blob/main/schemas/devContainer.base.schema.json), [containers.dev/supporting](https://containers.dev/supporting)
- GitHub Codespaces docs: [Introduction to dev containers](https://docs.github.com/en/codespaces/setting-up-your-project-for-codespaces/adding-a-dev-container-configuration/introduction-to-dev-containers), [Forwarding ports](https://docs.github.com/en/codespaces/developing-in-a-codespace/forwarding-ports-in-your-codespace), [Account-specific secrets](https://docs.github.com/en/codespaces/managing-your-codespaces/managing-your-account-specific-secrets-for-github-codespaces)

Unless shown in quotation marks, the doc citations below are paraphrases. Code citations are `path:line` at the commits above.

## 1. @devcontainers/cli on a headless VM

### What it is

- containers.dev/supporting: "The Dev Container Command Line Interface (CLI) is a reference implementation for the Dev Container Spec."
- `package.json`: `"engines": { "node": ">=20.0.0" }`; bin `devcontainer`.

### Install requirements on the VM

- Standalone installer (README "Install script"). It downloads a bundled Node.js runtime, "so no pre-installed Node.js is required". It supports Linux and macOS on x64 and arm64:
  ```
  curl -fsSL https://raw.githubusercontent.com/devcontainers/cli/main/scripts/install.sh | sh
  export PATH="$HOME/.devcontainers/bin:$PATH"
  ```
- Alternative: `npm install -g @devcontainers/cli`. The README notes that npm install needs Python and C/C++ build tools for a native dependency.
- Docker engine and CLI on the same host. The CLI shells out to `docker` (`--docker-path`) and to `docker compose` for compose configs (`--docker-compose-path`).
- BuildKit for Features. When building Features, the CLI emits `# syntax=docker/dockerfile:1.4` (`src/spec-node/containerFeatures.ts:275`). A current Docker Engine from get.docker.com uses BuildKit by default, so this is a non-issue on a fresh VM.
- No display, VS Code or other IDE component is needed.

### Commands relevant to devcrate

The commands are registered in `src/spec-node/devContainersSpecCLI.ts:95-116`.

| Command | Use for devcrate |
|---|---|
| `up` | Build or pull the image, apply Features, create/start the container, run lifecycle hooks. Prints a JSON result (`outcome`, container id, remote user, remote workspace folder). |
| `exec` | Run a command (or shell) in the running container as `remoteUser` with `remoteEnv` applied. This is the entry point for SSH sessions into the container. |
| `run-user-commands` | Re-run lifecycle hooks on an existing container. |
| `read-configuration --include-merged-configuration` | Read the merged config, including `forwardPorts` and `portsAttributes` contributed by Features or image metadata (`src/spec-node/imageMetadata.ts:36-38, 192-209`). |
| `set-up` | Treat an already-running container as a dev container. |
| `build` | Pre-build an image (relevant to prebuild/caching, which is out of scope for this ticket). |
| `stop` / `down` | **Not implemented.** They are unchecked in README lines 22-23. Use `docker stop`/`docker rm`, found by the CLI's `--id-label` labels. |

Useful `up` flags (`devContainersSpecCLI.ts:132-179`):

- `--workspace-folder`
- `--id-label` (labels used to find the container again; by default inferred from the workspace path)
- `--mount type=bind|volume,source=..,target=..`
- `--remote-env name=value`
- `--secrets-file <json>`
- `--additional-features <json>`
- `--dotfiles-repository`
- `--override-config`
- `--remove-existing-container`
- `--expect-existing-container`
- `--skip-post-create`
- `--skip-non-blocking-commands`
- `--log-format json`

### Lifecycle hooks

All six hooks are implemented in `src/spec-common/injectHeadless.ts:368-397`: `initializeCommand`, `onCreateCommand`, `updateContentCommand`, `postCreateCommand`, `postStartCommand` and `postAttachCommand`.

"Run once" behaviour uses marker files in the container's user data folder:

- onCreate, updateContent and postCreate markers are keyed on the container's `createdAt` (`injectHeadless.ts:432-433`).
- The postStart marker is keyed on `startedAt` (`injectHeadless.ts:438-439`).

Consequences:

- Destroying and recreating the container re-runs every hook.
- Restarting a stopped container re-runs only `postStartCommand` (plus `postAttachCommand`).
- With devcrate's stop model (destroy the droplet, keep state), a resume creates a fresh container, so `postCreateCommand` runs on every resume. The exception is when the container itself (not just a volume) survives, for example on a snapshot image.

`initializeCommand` runs on the host where the CLI runs (`src/spec-node/configContainer.ts:66`). The spec schema describes it as "A command to run locally (i.e Your host machine, cloud VM) before anything else." On a remote VM it therefore runs on the VM. If it ran on the laptop, it would need a separate code path in devcrate. DevPod's VM flow also runs it on the VM (see §2).

### Port forwarding: not implemented by the CLI

- A grep for `forwardPorts` in `cli/src` only matches the type, the metadata-merge code (`imageMetadata.ts`, `spec-configuration/configuration.ts:45`) and test fixtures. No code opens a forward.
- `appPort` is the only port property the CLI acts on. It is turned into Docker published ports (`src/spec-node/singleContainer.ts:346-347`).
- The spec defines forwarding as tool-level. JSON reference: `forwardPorts` are ports that "should always be forwarded from inside the primary container to the local machine". Unlike `appPort`, forwarded ports look like localhost to the application, so the app does not need to listen on all interfaces. Schema: "Ports that are forwarded from the container to the local machine. Can be an integer port number, or a string of the format "host:port_number"."

### Credentials: nothing built in

- A grep for `SSH_AUTH_SOCK` in `cli/src` finds no matches. The CLI does no SSH-agent or git-credential forwarding.
- Mechanisms the CLI does offer:
  - `--mount` adds bind/volume mounts, for example the VM's agent socket.
  - `--remote-env` sets env vars for lifecycle hooks and `exec`. It is accepted by `up`, `set-up`, `run-user-commands` and `exec` (`devContainersSpecCLI.ts:160, 405, 853, 1325`).
  - `--secrets-file` reads a JSON file of secret env vars (`devContainersSpecCLI.ts:1509`). These are passed to lifecycle hooks and dotfiles install (`injectHeadless.ts:348-397`). It is accepted by `up` and `run-user-commands` only, **not** `exec` (`devContainersSpecCLI.ts:172, 860`).

## 2. How DevPod does it

DevPod does **not** depend on `@devcontainers/cli`. A grep for `devcontainers/cli` in `pkg/` and `cmd/` finds nothing. The spec is reimplemented in Go under `pkg/devcontainer`: config, merge, Features (`pkg/devcontainer/feature`), build, compose and lifecycle hooks.

With a VM ("machine") provider, the flow is as follows:

1. The local CLI provisions the machine, then runs the DevPod agent on it. The agent's `devpod agent workspace up` calls the devcontainer runner **on the VM** (`cmd/agent/workspace/up.go:131`). The runner's `Up` runs `initializeCommand` with `sh -c` wherever the runner is (`pkg/devcontainer/run.go:103, 180-215`), which is the VM in this flow.
2. The agent installs Docker if `docker` is missing, using an embedded copy of the get.docker.com script (`cmd/agent/workspace/up.go:212-219, 434-447`; `scripts/install_docker.sh`). This can be turned off with the provider option `Agent.Docker.Install == "false"`.
3. Credentials. The agent first fetches docker/git credentials from the client over its tunnel (`cmd/agent/workspace/up.go:206`, `configureCredentials`).
4. Once the container is up, DevPod injects its own helper binary into it and runs `devpod agent container setup` as root (`pkg/devcontainer/setup.go:36-41, 105-120`). The setup step:
   - chowns the workspace;
   - writes `remoteEnv` into `/etc/environment` and `/etc/profile` (`pkg/devcontainer/setup/setup.go:40-56`);
   - optionally injects git credentials (`--inject-git-credentials`);
   - runs the lifecycle hooks (`pkg/devcontainer/setup/lifecyclehooks.go:23-70`).

   DevPod's hook semantics mirror the CLI's. Markers are keyed on container `Created` for onCreate/updateContent/postCreate and on `StartedAt` for postStart. postAttach always runs. A comment in that file says updateContent is not yet re-run on content change ("TODO: rerun when contents changed").
5. Port forwarding. DevPod opens an SSH connection **into the container**, served by its own in-container SSH server. Over that connection it forwards:
   - extra `-L` ports;
   - `appPort`;
   - merged `forwardPorts`, each to `localhost:<port>` on the laptop (`pkg/tunnel/services.go:160-220`).

   Its merge also concatenates `forwardPorts` from image metadata and Features (`pkg/devcontainer/config/merge.go:56, 105-115`).
6. SSH agent. DevPod's SSH server sets `SSH_AUTH_SOCK` for sessions it serves (`pkg/ssh/server/ssh.go:134`). Container setup chowns the agent socket directory to `remoteUser` (`pkg/devcontainer/setup/setup.go:244-254`). Docker credentials are forwarded through a credential helper over the tunnel (`AgentInjectDockerCredentials`, `cmd/up.go:577`).

Takeaway: DevPod has to own the in-container agent because it forwards ports and credentials into the container itself. With `@devcontainers/cli`, devcrate can skip the reimplementation. It still needs some way to reach container ports and to get the agent socket into the container.

## 3. How GitHub Codespaces does it

- "Whenever you work in a codespace, you are using a dev container on a virtual machine." If there is no `image` or Dockerfile, a default Linux image is used (Codespaces intro doc).
- Ports:
  - A process that prints a localhost URL gets auto-forwarded.
  - `forwardPorts` in devcontainer.json are forwarded for every codespace from the repo.
  - `gh codespace ports forward` forwards from the terminal.
  - In VS Code desktop, ports also appear on local `127.0.0.1` (Forwarding ports doc).
- Secrets are "exported as an environment variable into the user's terminal session". They are available in lifecycle scripts that run after the codespace is running. They are **not** available at build time (Dockerfile, custom entrypoint) or inside a dev container Feature (secrets doc).
- Spec gaps listed for Codespaces on containers.dev/supporting:
  - bind `mounts` are ignored except the Docker socket;
  - the `"host:port"` form of `forwardPorts`/`portsAttributes` is not supported;
  - `shutdownAction` does not apply;
  - `${localEnv:VARIABLE_NAME}` refers to the cloud host, not your machine.

This is the same situation devcrate is in.

## 4. devcontainer.json fields that matter for devcrate

### Port forwarding

| Field | Who acts on it | devcrate implication |
|---|---|---|
| `forwardPorts` | Tool. The CLI ignores it. | devcrate must read it, preferably from `read-configuration --include-merged-configuration` so Feature-contributed ports are included, then forward each port to laptop `localhost`. Decide whether to support `"service:port"` entries (Codespaces does not). |
| `portsAttributes`, `otherPortsAttributes` | Tool | Labels and `onAutoForward` behaviour. Optional for the MVP. |
| `appPort` | CLI, as Docker `-p` publish on the VM | The port is published on the VM's interfaces (exposed publicly unless bound to 127.0.0.1 or firewalled). devcrate should either not honour it as-is or bind it to 127.0.0.1. |
| `runArgs` | CLI, as Docker args | A user can add `-p`/`--network`. devcrate can also inject options through `--override-config`. |

### Credentials and environment

| Field | Who acts on it | devcrate implication |
|---|---|---|
| `remoteEnv` | Tool/CLI, for hooks and `exec` sessions | Natural home for opt-in env forwarding. Also available as the `--remote-env` flag, which avoids editing the user's file. |
| `containerEnv` | Docker, at container create | Static for the container's life. Changing it needs a rebuild. Do not put forwarded secrets here, because they persist in container metadata. |
| `mounts` | Docker | Host-side bind mounts. `--mount` is how to bring the VM's SSH-agent socket or forwarded files into the container. |
| `remoteUser` / `containerUser` / `updateRemoteUserUID` | CLI | Decide which user `exec` and the hooks run as. The forwarded agent socket and files must be readable by `remoteUser`. DevPod chowns the socket directory. |
| `userEnvProbe` | CLI | Controls the shell used to probe the user env for hooks and `exec`. The default is `loginInteractiveShell`. |
| `initializeCommand` | Host = VM | Runs on the VM, not the laptop. Document this. |
| `${localEnv:VAR}`, `${localWorkspaceFolder}` | Substituted on the CLI's host | Resolves on the **VM**. A config that expects laptop env vars will see blanks unless devcrate exports the opted-in vars into the CLI's environment on the VM. |
| `secrets` | Tool (recommendations only) | Schema: "Recommended secrets for this dev container. Recommendations are provided as environment variable keys with optional metadata." devcrate could use it to suggest which env vars to forward. |
| `postCreateCommand` etc. | CLI | Secrets passed via `--secrets-file` reach the hooks. Like Codespaces, they do not reach the image build or Features. |

## 5. Options for devcrate's port forwarding and SSH agent (inputs for #9 and #10)

This ticket does not choose among them.

**Port forwarding.** devcrate's SSH tunnel ends on the VM, and the CLI does not forward ports. Three ways to reach a container port:

- **(a) Publish to VM loopback.** At `up` time, publish each `forwardPorts` entry as `127.0.0.1:<p>:<p>` (through `--override-config` adding `appPort`/`runArgs`). Then `ssh -L <p>:127.0.0.1:<p>` to the VM. This is simple, but ports must be known before container create, and changing them means recreating the container.
- **(b) Forward to the container IP.** Use `ssh -L <p>:<container-ip>:<p>` with the IP from `docker inspect` on the bridge network. Nothing is published, and ports can be added at runtime. The app must listen on `0.0.0.0` in the container, not `127.0.0.1`. Spec-wise, `forwardPorts` should work even for apps bound to localhost.
- **(c) Forward from inside the container's network namespace.** Run a small forwarder from the devcrate agent (`docker exec` a relay, or `nsenter` into the container's netns). DevPod does this with an in-container SSH server. This is the most faithful to the spec, because it works for apps bound to localhost. It is also the most code.

**SSH agent.** The VM's `SSH_AUTH_SOCK` path changes on every SSH session. For a bind mount to stay valid across reconnects:

- point a stable path on the VM (for example a symlink kept up to date by the session) at the session's socket;
- bind-mount that stable path's directory with `--mount`;
- set `SSH_AUTH_SOCK` with `--remote-env`;
- make sure `remoteUser` can read the socket.

**Opt-in env.** Use `--remote-env` on `up` and `exec` for non-secret values. For secrets, use a `--secrets-file` written to the VM with mode 0600 for hooks, plus `--remote-env` on `exec` for interactive sessions (`exec` does not take `--secrets-file`).

**Opt-in files.** Either copy them to the VM and bind-mount them with `--mount`, or write them into the container with `exec` after `up`.
