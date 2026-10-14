# Passing secrets into devcontainer image builds

Research for [#17](https://github.com/ianmurrays/devcrate/issues/17), child of map [#1](https://github.com/ianmurrays/devcrate/issues/1). Feeds the grilling ticket [#18](https://github.com/ianmurrays/devcrate/issues/18).

**Question:** How can secrets reach a devcontainer image build (Dockerfile `RUN` steps and Features) without being baked into image layers? Cover BuildKit build secrets (`RUN --mount=type=secret`, `--mount=type=ssh`) and whether `@devcontainers/cli` (`devcontainer up`/`build`) can pass them through (flags, `build.options` in `devcontainer.json`, env); how Features receive options and whether any secret mechanism exists for them; how Codespaces, DevPod and Gitpod expose user/repo secrets at build time vs runtime; and the risk of passing secrets as build args. Give exact commands and the `devcontainer.json` keys involved.

## Answer

- **The mechanism is BuildKit secret and SSH mounts.** The Dockerfile opts in per `RUN` step with `RUN --mount=type=secret,id=<ID>` (file at `/run/secrets/<ID>`, or `env=<VAR>`), or with `RUN --mount=type=ssh`. The build client supplies values with `docker buildx build --secret id=<ID>[,env=<VAR>|,src=<file>]` and `--ssh default[=<socket>]`. The value exists only for that `RUN` step and is not written to a layer. Tested below: the secret appears nowhere in the image (layers, config, history).
- **`@devcontainers/cli` has no secret flag for builds.** Neither `up` nor `build` has `--secret`, `--ssh` or `--build-option`. The only route is **`build.options`** in `devcontainer.json`: the CLI appends those strings verbatim to `docker buildx build`. Example: `"build": {"dockerfile": "Dockerfile", "options": ["--secret", "id=NPM_TOKEN", "--ssh", "default"]}`. Values then come from the environment (or files) of the process that runs `devcontainer`, because the CLI runs `docker` with its own `process.env`. `--ssh default` uses `SSH_AUTH_SOCK` from that environment. Verified end to end with CLI 0.89.0.
- **`build.options` only applies to Dockerfile-based configs** (`build.dockerfile`/`dockerFile`). Image-based configs have no `build` section. Compose-based configs ignore it, but Compose has its own `build.secrets` / `build.ssh` keys in the compose file, and those work through `devcontainer up`/`build` (tested).
- **`--secrets-file` does not reach the build.** It feeds only lifecycle hooks and dotfiles, and it masks the values in the CLI's log output. It is accepted by `up` and `run-user-commands`, not by `build`.
- **To inject `build.options` without editing the repo:** `devcontainer up --override-config <file>`. It **replaces** the repo's `devcontainer.json` wholesale (no merge). Relative paths still resolve against the original file's folder. devcrate would have to read the repo's JSONC, add `build.options`, write the result and pass it. Tested. `devcontainer build` has no `--override-config`.
- **Features have no secret mechanism.** The spec and CLI pass Feature options as plain env vars (`devcontainer-features.env`, sourced by the install wrapper). The CLI-generated `RUN` lines for Features carry only a bind mount for the Feature content, never a secret or SSH mount. So even when `--secret`/`--ssh` reach the build, a Feature's `install.sh` sees neither (tested). Secrets passed as Feature options leak: the wrapper prints every option value into the build log, and the env file is written to the host's temp dir with mode 0644 (tested). On the non-BuildKit path the CLI `COPY`s that env file into a layer and never deletes it (code read, not tested).
- **Build args are not safe.** Docker: "Build arguments are visible in the `docker history` command and in `max` mode provenance attestations." Tested: a value passed through `build.args` (including `${localEnv:VAR}`) appears in the image config, once per `RUN` history entry after the `ARG` (9 times in the test image). It ships with the image to any registry it is pushed to.
- **Platforms:**
  - **Codespaces:** no build-time secrets. Secrets "cannot be used: During codespace build time (that is, within a Dockerfile or custom entry point)" nor "Within a dev container feature". Prebuilds see repo/org secrets in lifecycle commands only, never user secrets. The `<*>_CONTAINER_REGISTRY_*` secrets are image-pull credentials, a separate mechanism.
  - **DevPod:** honours `build.options` only on its `docker buildx` path. Its fallback internal-BuildKit path drops them (`// TODO: convert options.CliOpts`) and attaches only a registry-auth provider. There is no DevPod-level build-secret feature or documentation, and its Feature `RUN` lines have no mounts either.
  - **Gitpod Classic:** no. The image builder runs `buildctl` with no `--secret`/`--ssh`/build-arg options, and the docs list "Gitpod environment variables" under "Not available" at build time.
  - **Ona (formerly Gitpod, devcontainer-based):** yes, for **organization and project** secrets only ("User secrets are not available during builds"). It passes them as BuildKit secrets automatically, id = env var name or file path, and the Dockerfile opts in per `RUN` step. Dockerfile configs only; Compose "not yet supported". The docs warn that images are cached and shared across the project.

**For devcrate (input to #18, not a decision):** the build already runs in `up`'s SSH session with agent forwarding (#9). So `--ssh default` works today for any repo whose `devcontainer.json` declares it, and `forward_env` values can be made visible to `--secret id=NAME` by exporting them into the `devcontainer up` process env. Two things for #18 to decide: who writes `build.options` (the repo, or devcrate via `--override-config`), and whether the `devcontainer up` env must be scrubbed to the allowed list. Buildx auto-detects `type=env` for any `id` that matches a set env var, so a repo-declared `--secret id=X` can read any variable present in that process. A repo can also name `src=<any path>` readable by the CLI user. That is not a new trust boundary on its own, since `initializeCommand` already runs repo code on the VM (#4). Images built with secret mounts stay safe to cache or push (#16), unless a `RUN` step writes the secret to disk itself.

## Sources

- devcontainers/cli @ [`155f8b29`](https://github.com/devcontainers/cli/tree/155f8b29e1c521bf365e2aebd74f3258a9160718) (2026-10-02), v0.89.0. Tests ran with `npx @devcontainers/cli@0.89.0`.
- devcontainers/spec @ [`c95ffeed`](https://github.com/devcontainers/spec/tree/c95ffeed1d059abfe9ffbe79762dc2fa4e7c2421): `schemas/devContainer.base.schema.json`, `docs/specs/devcontainerjson-reference.md`, `docs/specs/secrets-support.md`, `docs/specs/declarative-secrets.md`, `proposals/build-options.md`
- docker/docs @ [`9643ba08`](https://github.com/docker/docs/tree/9643ba0813ea4290a7ab1f206dddaf7683f39edd): [Build secrets](https://docs.docker.com/build/building/secrets/), [Compose build reference](https://docs.docker.com/reference/compose-file/build/), [Compose secrets](https://docs.docker.com/reference/compose-file/secrets/)
- moby/buildkit @ [`8e20b411`](https://github.com/moby/buildkit/tree/8e20b4110a5482b200bebaf5e3d667a8f4446ac1): `frontend/dockerfile/docs/reference.md` ([Dockerfile reference](https://docs.docker.com/reference/dockerfile/)), `session/sshforward/sshprovider/agentprovider.go`
- docker/buildx @ [`c43f837d`](https://github.com/docker/buildx/tree/c43f837d31f1d1d7cef75a9cbb96585a19ef46b4): `docs/reference/buildx_build.md` ([`docker buildx build`](https://docs.docker.com/reference/cli/docker/buildx/build/))
- github/docs @ [`ba6f6f9b`](https://github.com/github/docs/tree/ba6f6f9b9b31c4f0a991fbcb1005080526424804): [Account-specific secrets](https://docs.github.com/en/codespaces/managing-your-codespaces/managing-your-account-specific-secrets-for-github-codespaces), [Configuring prebuilds](https://docs.github.com/en/codespaces/prebuilding-your-codespaces/configuring-prebuilds), [Private registry access](https://docs.github.com/en/codespaces/reference/allowing-your-codespace-to-access-a-private-registry)
- loft-sh/devpod @ [`5a0efcbf`](https://github.com/loft-sh/devpod/tree/5a0efcbff6610ab114b421f68a890739a452e66b)
- gitpod-io/gitpod (Gitpod Classic) @ [`3e9ff353`](https://github.com/gitpod-io/gitpod/tree/3e9ff3533fb6046fea96f0960896dffc7c66c972)
- [containers.dev/supporting](https://containers.dev/supporting) (Codespaces limitations list), fetched 2026-10-14
- Ona docs, fetched 2026-10-14: [Dev Container overview, "Using secrets during builds"](https://ona.com/docs/ona/configuration/devcontainer/overview#using-secrets-during-builds), [How secrets work](https://ona.com/docs/ona/configuration/secrets/overview), [Gitpod Classic: Workspace Image](https://ona.com/docs/configure/workspaces/workspace-image) (`gitpod.io/docs/...` now redirects there)

Unless shown in quotation marks, doc citations are paraphrases. Code citations are `path:line` at the commits above.

## 1. BuildKit build secrets (Docker docs)

**Why not build args or ENV.** "Build arguments and environment variables are inappropriate for passing secrets to your build, because they persist in the final image. Instead, you should use secret mounts or SSH mounts" (Build secrets). The Dockerfile reference `ARG` warning: "It isn't recommended to use build arguments for passing secrets such as user credentials, API tokens, etc. Build arguments are visible in the `docker history` command and in `max` mode provenance attestations" (`reference.md:2557-2565`). The predefined proxy args (`HTTP_PROXY` etc.) are the only args excluded from `docker history` (`reference.md:2700-2702`).

**Client side, `docker buildx build`** (`buildx_build.md:958-1080`):

```console
# from an env var: id doubles as the variable name if it is set (type auto-detected)
$ NPM_TOKEN=... docker buildx build --secret id=NPM_TOKEN .
# from a differently named env var
$ docker buildx build --secret id=npm,env=NPM_TOKEN .
# from a file
$ docker buildx build --secret id=aws,src=$HOME/.aws/credentials .
# SSH agent
$ docker buildx build --ssh default=$SSH_AUTH_SOCK .
```

- Type auto-detection: "If an environment variable with the same key as `id` is set, then Buildx uses `type=env` … If no such environment variable is set, and `type` is not set, then Buildx falls back to `type=file`" (`buildx_build.md:975-978`).
- `--ssh default` with no path uses `SSH_AUTH_SOCK` of the client process: `conf.Paths = []string{os.Getenv("SSH_AUTH_SOCK")}` (`agentprovider.go:31-33`).

**Dockerfile side** (`reference.md:931-1034`):

```dockerfile
RUN --mount=type=secret,id=NPM_TOKEN,env=NPM_TOKEN npm ci        # as env var (Dockerfile frontend >= 1.10.0)
RUN --mount=type=secret,id=aws,target=/root/.aws/credentials aws s3 cp ...   # as file
RUN --mount=type=secret,id=NPM_TOKEN,required=true cat /run/secrets/NPM_TOKEN # fail if missing
RUN --mount=type=ssh git clone git@github.com:org/private.git
```

- `type=secret` options: `id`, `target` (default `/run/secrets/<id>`), `env` ("since Dockerfile v1.10.0"), `required` ("Defaults to `false`"), `mode` (default `0400`), `uid`, `gid` (`reference.md:939-947`). With `required=false`, a missing secret means no file, not an error.
- `type=ssh` options: `id` (default `default`), `target` (default `/run/buildkit/ssh_agent.${N}`), `required`, `mode` (`0600`), `uid`, `gid` (`reference.md:1004-1011`).
- BuildKit also has predefined `GIT_AUTH_TOKEN` / `GIT_AUTH_HEADER` secrets for `ADD <git url>`, and `HTTP_AUTH_TOKEN_<host>` for `ADD`/`COPY` over HTTP (Build secrets).
- A secret mount does not stop a `RUN` step from writing the value to disk, for example `npm config set`. Ona's docs call this out explicitly (§5).

**Compose** (Compose build reference, `secrets` and `ssh`):

```yaml
services:
  dev:
    build:
      context: .
      secrets: [npm_token]     # Dockerfile: RUN --mount=type=secret,id=npm_token
      ssh: [default]           # Dockerfile: RUN --mount=type=ssh
secrets:
  npm_token:
    environment: NPM_TOKEN     # or file: ./path
```

## 2. `@devcontainers/cli`

### What it passes to `docker buildx build`

`buildAndExtendImage` (`src/spec-node/singleContainer.ts:124-273`) assembles the build command for Dockerfile-based configs:

- `config.build.args` become `--build-arg K=V` (`:238-243`).
- `config.build.options` are appended verbatim: `args.push(...buildOptions)` (`:244-247`).
- Then the Feature build args and contexts (`:248`), then the context path.

The `options` field exists only on the Dockerfile config variants (`src/spec-configuration/configuration.ts:115-135`). `build.options` is read nowhere else: `grep build?.options` finds only `singleContainer.ts:244`. The image-plus-Features path (`extendImage` in `src/spec-node/containerFeatures.ts:31-131`) builds its own arg list with no user options. The Compose path writes an override file with only `image`, `dockerfile`, `target`, `context`, `args` and `additional_contexts` (`src/spec-node/dockerCompose.ts:196-258`), so `build.secrets`/`build.ssh` from the user's compose file pass through untouched.

Spec: `build.options` is "An array of Docker image build options that should be passed to the build command when building a Dockerfile" (`devcontainerjson-reference.md:44`). The schema says "Additional arguments passed to the build command." (`devContainer.base.schema.json:637-643`). The proposal: "all entries from the `options` are passed to the `build` command after all other options" (`proposals/build-options.md`). Its examples are `--add-host` and `--network=host`, and it says nothing about secrets.

### Where values come from

- `docker` is spawned with the CLI's own environment: `dockerEnv: cliHost.env` (`src/spec-node/devContainers.ts:243`), and `env: process.env` (`src/spec-common/cliHost.ts:71`). So `--secret id=X` resolves `X` from the env of the `devcontainer` process, and `--ssh default` uses its `SSH_AUTH_SOCK`.
- `${localEnv:VAR}` is substituted across the whole config from the same env (`src/spec-node/configContainer.ts:94-100`, `src/spec-common/variableSubstitution.ts:96-98`). This is fine inside `build.options` (`"id=npm,env=NPM_TOKEN"` needs no substitution at all) but leaky inside `build.args`.
- No `devcontainer.json` key carries secret values. The spec's `secrets` property is "Recommended secrets for this dev container … provided as environment variable keys with optional metadata" (`devContainer.base.schema.json:53-76`). The declarative-secrets spec adds only that implementations "*may* inject these secrets into the container as environment variables". The secrets-support spec says "Secrets are not part of dev containers specification" and describes runtime use "similar to `remoteEnv`". Neither mentions builds.

### Flags

- `up` options (`src/spec-node/devContainersSpecCLI.ts:133-179`) include `--secrets-file` (`:172`), `--override-config` (`:145`), `--remote-env`, `--cache-from`/`--cache-to` and `--buildkit auto|never`. There is no `--secret`, `--ssh` or build-option passthrough.
- `build` options (`:564-590`) have neither `--secrets-file` nor `--override-config`.
- `--secrets-file` values go to lifecycle hooks and dotfiles (`src/spec-common/injectHeadless.ts:348-410`) and to log masking (`src/spec-node/devContainers.ts:118, 302-306`). They are never added to the build command.
- `--override-config`: `documents.readDocument(overrideConfigFile ?? configFile)` reads the override **instead of** the repo file (`src/spec-node/configContainer.ts:84`). `config.configFilePath = configFile` (`:109`) keeps relative paths anchored at the repo's `.devcontainer/`.

### Image metadata

The `devcontainer.metadata` label copies a fixed property list (`src/spec-node/imageMetadata.ts:17-42`). It does not include `build`, so `build.options`/`build.args` don't land in the label. The test image's label was `[ {"id":"./probe"} ]`.

### Syntax directive gotcha (old engines only)

When Features are present and the Docker Engine is older than 23.0.0, the CLI prepends `# syntax=docker/dockerfile:1.4` to the merged Dockerfile, unless the user's own `# syntax=` already names a version >= 1.4 (`src/spec-node/containerFeatures.ts:248-250, 271-276`; `src/spec-node/dockerfileUtils.ts:263-273`). Frontend 1.4 predates `env=` on secret mounts (1.10.0). On Engine >= 23 no directive is added. In the test (Engine 29.4.0), `env=` worked without any `# syntax` line. Putting `# syntax=docker/dockerfile:1` first in the Dockerfile avoids the issue on any engine.

## 3. Features

- Options reach `install.sh` only as env vars. The CLI writes `devcontainer-features.env` per Feature (`src/spec-node/containerFeatures.ts:280-303`). The generated wrapper prints them (`echo 'Options       :'` / `echo '${optionsIndented}'`) and then sources them with `set -a; . ./devcontainer-features.env` (`src/spec-configuration/containerFeaturesConfiguration.ts:237-285`).
- The `RUN` lines for Features carry only `--mount=type=bind,from=dev_containers_feature_content_source,...` on the BuildKit path, and a plain `COPY` on the non-BuildKit path (`containerFeaturesConfiguration.ts:300-361`). There is no secret or SSH mount, and no way for a Feature or the user to add one. The BuildKit path ends with `rm -rf ${dest}`. The non-BuildKit path `COPY`s the folder, env file included, into a layer and never removes it (code read only, not tested).
- The Features spec (`docs/specs/devcontainer-features.md`) has no secret concept. `grep -i 'secret\|password\|token'` finds nothing.
- Conclusion: there is no supported way to give a Feature a secret. Anything a Feature needs credentials for has to move into a Dockerfile `RUN --mount=type=secret` step or into a lifecycle hook (`--secrets-file`/`--remote-env`).

## 4. Empirical test

Environment: macOS, Docker Engine 29.4.0 (OrbStack), buildx v0.37.1, `npx @devcontainers/cli@0.89.0`. A throwaway `ssh-agent` with a generated ed25519 key. Fixtures were in a scratch dir, not this repo.

**Fixture A (Dockerfile + local Feature):**

```dockerfile
FROM alpine:3.20
ARG LEAKY
RUN apk add --no-cache openssh-client
RUN --mount=type=secret,id=TOKEN test "$(sha256sum < /run/secrets/TOKEN | cut -d' ' -f1)" = <sha256 of hunter2> && echo SECRET-FILE-OK
RUN --mount=type=secret,id=TOKEN,env=TOKEN_ENV test "$(printf %s "$TOKEN_ENV" | sha256sum | cut -d' ' -f1)" = <sha256 of hunter2> && echo SECRET-ENV-OK
RUN --mount=type=ssh ssh-add -l && echo SSH-OK
RUN ls -la /run/secrets 2>&1 || true
```

```json
{
  "build": {
    "dockerfile": "Dockerfile",
    "args": { "LEAKY": "${localEnv:LEAKY}" },
    "options": ["--secret", "id=TOKEN", "--ssh", "default"]
  },
  "features": { "./probe": { "token": "feature-opt-s3cret" } }
}
```

The Feature `./probe` declares a string option `token`. Its `install.sh` prints `$TOKEN` and checks for `/run/secrets/TOKEN` and `SSH_AUTH_SOCK`.

```console
$ TOKEN=hunter2 LEAKY=buildarg-s3cret npx @devcontainers/cli@0.89.0 build --workspace-folder . --image-name probe:b
```

The CLI ran `docker buildx build --load --build-arg BUILDKIT_INLINE_CACHE=1 -f …/Dockerfile-with-features -t probe:b --target dev_containers_target_stage --build-arg LEAKY=buildarg-s3cret --secret id=TOKEN --ssh default --build-context … .devcontainer`. Results:

| Check | Result |
|---|---|
| `RUN --mount=type=secret,id=TOKEN` (file) | `SECRET-FILE-OK` |
| `RUN --mount=type=secret,id=TOKEN,env=TOKEN_ENV`, no `# syntax` line | `SECRET-ENV-OK`. The build log shows the variable as `****`. |
| `RUN --mount=type=ssh` | `SSH-OK` |
| `/run/secrets` in a later plain `RUN` | `No such file or directory` |
| Feature `install.sh` | `TOKEN option env is 'feature-opt-s3cret'`, `no /run/secrets/TOKEN`, `no SSH_AUTH_SOCK` |
| Build log | `Options : TOKEN="feature-opt-s3cret"` printed by the wrapper |
| `docker history --no-trunc`: `hunter2` | 0 hits |
| `docker history --no-trunc`: `buildarg-s3cret` | 9 hits (`ARG LEAKY=buildarg-s3cret` plus every later `RUN \|N LEAKY=buildarg-s3cret …`) |
| `docker save` then grep of every blob (layers + config) | `hunter2`: 0 blobs. `buildarg-s3cret`: 1 blob (the image config). `feature-opt-s3cret`: 0 blobs. |
| `devcontainer.metadata` label | `[ {"id":"./probe"} ]` |
| Host temp dir `…/devcontainercli/container-features/0.89.0-*/probe_0/devcontainer-features.env` | contains `feature-opt-s3cret`, mode `-rw-r--r--` |

The first run of fixture A used `test "$(cat …)" = hunter2`. That put the literal into the `RUN` command text and so into `docker history`. A Dockerfile that compares against a literal secret leaks it through its own source, not through the mount.

**`--secrets-file` masking:** running the same fixture with `up --secrets-file` holding `{"X":"feature-opt-s3cret"}` turned the wrapper's log line into `TOKEN="********"` and the Feature's echo into `'********'`. Masking applies to everything the CLI logs, build output included. It does not pass anything to the build.

**Fixture B (`--override-config`):** the repo `devcontainer.json` was `{ "build": { "dockerfile": "Dockerfile" } }`, and the Dockerfile ran `RUN --mount=type=secret,id=TOKEN,required=true test -s /run/secrets/TOKEN`.

- `TOKEN=x devcontainer build …` gave `ERROR: secret TOKEN: not found`.
- `TOKEN=x devcontainer up --workspace-folder . --override-config /elsewhere/override.json` with `{ "build": { "dockerfile": "Dockerfile", "options": ["--secret","id=TOKEN"] } }` gave `OVERRIDE-SECRET-OK` and `"outcome":"success"`. The relative `Dockerfile` path resolved against the repo's `.devcontainer/`.

**Fixture C (Compose):** a compose service with `build.secrets: [npm_token]`, `build.ssh: [default]` and top-level `secrets.npm_token.environment: NPM_TOKEN`, plus the same local Feature. `NPM_TOKEN=x devcontainer build` gave `COMPOSE-SECRET-OK` and `COMPOSE-SSH-OK`. The Feature again saw `no /run/secrets/TOKEN`.

## 5. Platforms

### GitHub Codespaces

- "A development environment secret is exported as an environment variable into the user's terminal session." Secrets "cannot be used: During codespace build time (that is, within a Dockerfile or custom entry point). Within a dev container feature." They can be used "Within a dev container lifecycle script that is run after the codespace is running" (account-specific secrets doc, "Using secrets").
- Prebuilds: "To allow the prebuild process to access environment variables required to create your development environment, you can set these either as … repository secrets or as … organization secrets." Also: "Prebuilds cannot use any user-level secrets while building your environment, because these are not available until after the codespace has been created." (Configuring prebuilds, "Configuring environment variables").
- `<*>_CONTAINER_REGISTRY_SERVER` / `_USER` / `_PASSWORD` secrets let Codespaces access a private registry. Docker login is written to `~/.docker/config.json` "after the `onCreateCommand` lifecycle hook but before `postCreateCommand`" (private registry doc). These are pull credentials, not build secrets.
- Codespaces documents no way to pass `--secret` to its image build. The containers.dev "supporting" page for Codespaces doesn't list `build.options` among the unsupported properties. Whether Codespaces honours `build.options` is not documented, and there is no Codespaces secret source for it to reference anyway.

### DevPod

- DevPod parses `build.options` (`pkg/devcontainer/config/config.go:260-265`) into `CliOpts` (`pkg/devcontainer/build/options.go:61`).
- On the `docker buildx` path they are appended: `args = append(args, options.CliOpts...)` (`pkg/driver/docker/build.go:191`). The `docker` child process inherits the agent's env (`pkg/docker/helper.go:341-347`).
- When buildx is absent, or `ForceInternalBuildKit` is set, DevPod uses its embedded BuildKit client (`pkg/driver/docker/build.go:70-94`). That client attaches only `authprovider.NewDockerAuthProvider` and leaves `// TODO: convert options.CliOpts into a solveOptions.FrontendAttr` (`pkg/devcontainer/buildkit/buildkit.go:37-38, 113-114`). So `build.options` and any `--secret` are silently dropped there.
- The build runs on the remote machine under the DevPod agent. The repo documents no mechanism that puts laptop secrets or the forwarded SSH agent into that build environment. `docs/pages` has no build-secret page: grepping for `secret` finds only provider options.
- Feature layers are a plain `RUN cd /tmp/build-features/<i> && ./devcontainer-features-install.sh` with no mounts (`pkg/devcontainer/feature/extend.go:169-185`).

### Gitpod Classic (`.gitpod.yml` + `.gitpod.Dockerfile`)

- The image builder (`components/image-builder-bob/pkg/builder/builder.go:134-157`) runs `buildctl build --progress=plain --output=type=image,… --local=context=… --frontend=dockerfile.v0 --local=dockerfile=… --opt=filename=…`. It passes no `--secret`, `--ssh` or `build-arg` options. The `BuildRequest` API carries only source, registry auth and flags (`components/image-builder-api/imgbuilder.proto:66-74`).
- Docs ("Workspace Image", "What is available at build time"): "Not available: Gitpod environment variables, not available due to security reasons." The suggested workaround is to build manually with the Docker daemon inside a running workspace and pass the variable as a build arg.
- Gitpod Classic pay-as-you-go was sunset on 2025-10-15 (repo README).

### Ona (formerly Gitpod; devcontainer-based)

From "Using secrets during builds":

- "Organization and project secrets can be used during Dev Container image builds via secret mounts." Also: "Secrets are not exposed by default. Each RUN instruction in your Dockerfile must explicitly request the secrets it needs using the --mount=type=secret flag."
- "For file secrets, the id is the full file path. For environment variable secrets, the id is the variable name." Also: "No changes to devcontainer.json are needed."
- "User secrets are not available during builds. Built images are cached and shared across the project when the Dev Container image cache is enabled, so personal credentials in a build could be exposed to other team members."
- "Build-time secret mounts require a Dockerfile-based Dev Container … Docker Compose-based Dev Containers are not yet supported."
- Ona's docs require `# syntax=docker/dockerfile:1` for `env=` mounts. That depends on the frontend version their builder ships. On Docker Engine 29.4 it was not needed (§4).
- Features are not mentioned.

## 6. Open points / not verified

- The non-BuildKit (`--buildkit never`) Feature path leaving `devcontainer-features.env` in a layer comes from reading the code. It was not tested.
- Whether Codespaces passes `build.options` through is undocumented. It was not tested.
- DevPod was not run. Its behaviour above comes from source at `5a0efcbf`.
- Ona's behaviour comes from its public docs only. Its builder is not open source.
