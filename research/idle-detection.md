# How existing tools detect an idle dev environment

Research for [#5](https://github.com/ianmurrays/devcrate/issues/5). Sources accessed 2026-10-13. Source code pinned to DevPod `loft-sh/devpod@5a0efcbf` (2025-11-13) and Coder `coder/coder@db7f5a77` (2026-10-13).

## Answer

None of the four tools uses CPU, network, disk or process activity as its idle signal. Two families exist:

- **Connection presence** (DevPod, Coder, and SSH in Gitpod/Ona): the environment is busy while a client connection exists, whatever the user does inside it.
- **User input** (Codespaces, Gitpod Classic, the IDE side of Gitpod/Ona): keystrokes, mouse, and (Codespaces only) terminal input or output reset a timer.

Defaults: DevPod 20m in code, 10m in the DigitalOcean provider; Coder 1h activity bump on top of a template-set TTL; Codespaces 30m; Gitpod Classic 30m; Ona 30m. Long-running work with no client attached counts as idle in all four unless the user opts in to a keep-alive: DevPod only for `up`/`build`, Ona with `keep-alive`, Gitpod Classic with `gp timeout set`.

## DevPod

DevPod has two separate mechanisms. Both reduce to "is a laptop-side DevPod tunnel process alive".

### Machine providers (VM, the devcrate case)

- **Signal:** the mtime of `<agent dir>/contexts/*/workspaces/*/workspace.json` on the VM. The `devpod agent daemon` (installed on the VM) reads this mtime. Its description is "Watches for activity and stops the server due to inactivity" (`cmd/agent/daemon.go:35`).
- **Who emits it:** the laptop. While `devpod ssh` (also used as the SSH `ProxyCommand` by IDEs) is connected, the local container tunnel runs `agent workspace update-config` over SSH every 30s (`pkg/tunnel/container.go:28`, `:168`). That command rewrites `workspace.json` every time (`cmd/agent/workspace/update_config.go:39`, `pkg/agent/agent.go:251` `os.WriteFile`), which bumps the mtime. Each new connection also rewrites it via `agent container-tunnel` (`cmd/agent/container_tunnel.go:54`).
- **Check:** the daemon polls every 60s by default, set by `--interval` (`cmd/agent/daemon.go:66`). It takes the newest mtime across all workspaces on the machine. If `mtime + timeout` is in the past, it runs the provider's `agent.exec.shutdown` command (`cmd/agent/daemon.go:126-141`).
- **Defaults:** `DefaultInactivityTimeout = 20m` when `agent.inactivityTimeout` is unset (`pkg/agent/agent.go:25`). The docs say "All official DevPod providers offer this pre-configured to 5-10 minutes" ([docs](https://devpod.sh/docs/developing-in-workspaces/inactivity-timeout)). The DigitalOcean provider sets `INACTIVITY_TIMEOUT` to `10m` by default and shuts down with `${DO_PROVIDER} stop` ([provider.yaml](https://github.com/loft-sh/devpod-provider-digitalocean/blob/main/hack/provider/provider.yaml)).
- **Keep-alive for long work:** while `agent workspace up` or `build` runs, a `workspace.lock` file exists (`cmd/agent/workspace/up.go:74`, `build.go:53`). The daemon then adds a flat 20m to the observed mtime (`cmd/agent/daemon.go:217-218`). There is no keep-alive for user processes such as a build started in a shell.
- **Boot grace:** on start, the daemon touches every `workspace.json` (`initialTouch`, `cmd/agent/daemon.go:171`), so a freshly booted VM gets a full timeout window.
- **Opt-in:** a workspace with no `exec.shutdown` configured is ignored (`cmd/agent/daemon.go:205`).

### Container (non-machine) providers

- `devpod ssh` starts `helper ssh-server --track-activity` inside the container (`cmd/ssh.go:504`). While that process runs, it touches `/tmp/devpod.activity` every 10s (`cmd/helper/ssh_server.go:102-119`).
- The container daemon checks that file every 10s. When `mtime + containerInactivityTimeout` has passed, it exits. Because it runs as PID 1, the container stops (`cmd/agent/container/daemon.go:83`, `:199-211`). The docs describe this as killing "the process with pid 1" ([docs](https://devpod.sh/docs/developing-providers/agent)).

### Known false positives and negatives

- Open but unused IDE: an idle VS Code Remote-SSH window keeps the tunnel up, and the tunnel heartbeats every 30s, so the VM never stops (false negative). This follows from the code above. No doc states it.
- Laptop asleep, offline or closed during a long build: the tunnel dies and the heartbeat stops, so the VM shuts down after the timeout even though work is running (false positive). The `workspace.lock` grace only covers DevPod's own `up`/`build`.
- [loft-sh/devpod#795](https://github.com/loft-sh/devpod/issues/795): "Inactivity timeout stopping workspace although actively working in workspace" (v0.4.0, local Docker Desktop provider). Closed with no stated resolution.
- [loft-sh/devpod#741](https://github.com/loft-sh/devpod/issues/741): Kubernetes pods with a 1h timeout were still running after 2 days. The reporter suggests that VS Code reconnects through the DevPod proxy restart the pod.

## Coder

- **Signal:** session counts reported by the workspace agent. The agent classifies each SSH session by an app-name environment variable into families: SSH, VS Code, JetBrains, reconnecting PTY (web terminal) (`agent/agentssh/agentssh.go:448-470`). JetBrains is tracked through its single persistent TCP-forwarding channel, because "JetBrains launches hundreds of ssh sessions" (`agentssh.go:505-507`). It is also detected by process cmdline markers (`agentssh.go:47-50`).
- **Interval:** the agent reports stats every 5m by default. coderd can override this (`agent/stats.go:46`, `:94`).
- **Decision (server side):** if a report has no session counts, nothing happens. Otherwise coderd extends the workspace deadline by the template's activity bump (`coderd/workspacestats/reporter.go:157-194`). Docs: "The duration by which to extend a workspace's deadline when activity is detected (default: 1 hour)" ([template schedule docs](https://coder.com/docs/admin/templates/managing-templates/schedule)). The autostop TTL itself is set per template. The pages fetched do not state a default.
- **Counts:** VS Code/code-server, JetBrains, the web terminal, `coder ssh`/SSH config, and AI task "working" status ([workspace scheduling docs](https://coder.com/docs/user-guides/workspace-scheduling)).
- **Does not count:** the dashboard, settings, logs, "Accessing ports through direct URLs without an active session", and background agent stats reporting (same page).
- **Known issues:**
  - The docs tell users to close IDE windows and SSH sessions when done, because open sessions keep the workspace active (false negative for idle editors).
  - A deadline bump can cross an autostart boundary, which interacts badly with the TTL (comment in `coderd/workspacestats/activitybump.go:42-56`).
  - The Premium "autostop requirement" ignores activity entirely.

## Gitpod

Gitpod has two products with different rules.

### Gitpod Classic

([docs](https://ona.com/docs/classic/user/configure/workspaces/workspace-lifecycle))

- "By default, workspaces stop following 30 minutes without user input (e.g. keystrokes or terminal input commands)". The timeout can go up to 24h.
- "All inactivity timeouts are dependent on an active editor or IDE connection". "Closing your Gitpod connected editor or IDE will reduce the workspace timeout to 5 minutes" unless an explicit timeout is set.
- Extend with `gp timeout set`. Maximum lifetime: 8h on the free plan, 36h on paid plans.
- SSH input counts through ws-proxy heartbeats on SSH PTY channels: "input from an SSH session counts towards a workspace's activity and prevents its timeout" ([gitpod-io/gitpod#7760](https://github.com/gitpod-io/gitpod/pull/7760)).

### Ona (current Gitpod)

([environment timeout docs](https://ona.com/docs/ona/organizations/policies/environment-timeout.md))

- **Default:** 30m auto-stop.
- **SSH:** "Any open SSH connection (terminal, SCP, SFTP, tunnels) prevents the environment from going idle, regardless of user interaction. Connection presence alone is enough." There is one exception: "VS Code Desktop opens an SSH connection to the environment, but that connection is excluded from activity tracking". VS Code and JetBrains send keyboard/mouse-based activity signals instead.
- **Agents and processes:** the environment is not idle while an agent task executes. `ona environment keep-alive` prevents idle while a watched process runs.
- **Explicitly not activity:** "Background processes running inside the environment (builds, servers, cron jobs)", port-forward and exposed-port traffic, file system changes, and "CPU or memory usage".
- **Granularity:** "Activity is tracked in 5-minute intervals". With a 30m timeout, the stop may come around 35m.
- **Sleep:** "JS timers freeze during sleep". If the laptop sleeps longer than the timeout, the environment may stop before VS Code resumes.

## GitHub Codespaces

([timeout docs](https://docs.github.com/en/codespaces/setting-your-user-preferences/setting-your-timeout-period-for-github-codespaces), [lifecycle docs](https://docs.github.com/en/codespaces/about-codespaces/understanding-the-codespace-lifecycle))

- **Default:** 30m. The setting can range from 5m to 240m. An organization policy can cap it.
- **What counts:** "inactivity is defined as the absence of activity indicative of a user's presence. Personal interaction with a codespace, such as typing or using the mouse, resets the idle timeout period. Terminal activity, either input or output, also resets the idle timeout period."
- **Port traffic:** "if you share a port, and then don't interact with the codespace, and no terminal output is generated, the codespace will time out". Port traffic only counts indirectly, when it causes terminal output.
- **Closing the tab:** "Closing a codespace does not stop the codespace". It runs until the idle timeout fires.
- **Maximum lifetime:** "Regardless of your idle timeout setting, a codespace has a maximum lifetime of 12 hours."
- **Side effect of the output rule:** a build that prints to a terminal keeps the codespace alive. A silent one does not.

## Comparison

| Tool | Idle signal | Emitted by | Default | Long silent build | Open, idle IDE |
|---|---|---|---|---|---|
| DevPod (machine) | `workspace.json` mtime, touched every 30s by the connected laptop tunnel | Laptop | 20m in code, 10m DO provider | Stops (only `up`/`build` get +20m) | Keeps alive |
| Coder | Agent session counts (SSH/VS Code/JetBrains/PTY), reported every 5m | VM agent | 1h bump, template TTL | Stops if no session | Keeps alive |
| Gitpod Classic | User input via IDE, SSH PTY input | IDE / proxy | 30m; 5m after the IDE closes | Stops unless `gp timeout set` | Stops after 30m |
| Ona | SSH connection presence, IDE keyboard/mouse, agent tasks, `keep-alive` | Server / IDE | 30m, 5-min granularity | Stops unless `keep-alive` | Stops (VS Code SSH excluded) |
| Codespaces | Typing/mouse plus terminal input or output | Platform | 30m (5m-4h), 12h maximum | Stops if silent; alive if it prints | Stops |

## Relevance to devcrate

devcrate runs its idle check as an agent on the VM, with no server, and IDEs connect through a plain SSH config entry. The DevPod machine model fits most directly: a file mtime on the VM, bumped by activity, polled by a daemon that then runs a "stop" action. Unlike DevPod's tunnel heartbeat, an agent on the VM can see sshd sessions directly, like Coder's session counting, so devcrate does not need the laptop CLI to stay running. Because a plain SSH config entry makes VS Code's connection a normal SSH session, the Ona exclusion of VS Code SSH has no equivalent without IDE-side signals. Two failure modes are documented across all four tools: open but unused editors keep the environment alive, and silent long-running work with no client attached gets stopped. The common mitigation is an explicit opt-in keep-alive (Ona `keep-alive`, Gitpod `gp timeout set`), not CPU heuristics.
