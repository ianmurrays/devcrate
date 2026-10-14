# How tools auto-detect listening ports in remote environments

Research for [#19](https://github.com/ianmurrays/devcrate/issues/19). Sources accessed 2026-10-14. Source code pinned to VS Code `microsoft/vscode@76e9dff` (2026-10-14), DevPod `loft-sh/devpod@5a0efcb` (2025-11-14), Gitpod Classic `gitpod-io/gitpod@3e9ff35` (2026-10-13) and the dev container spec `devcontainers/spec@c95ffee` (2026-03-20). `file:line` references below are at those commits.

Permalink bases used below:

- VS Code: `https://github.com/microsoft/vscode/blob/76e9dff43b237f3fcbb8a289ed2f8bdf28105baf/`
- DevPod: `https://github.com/loft-sh/devpod/blob/5a0efcbff6610ab114b421f68a890739a452e66b/`
- Gitpod: `https://github.com/gitpod-io/gitpod/blob/3e9ff3533fb6046fea96f0960896dffc7c66c972/`
- Spec: `https://github.com/devcontainers/spec/blob/c95ffeed1d059abfe9ffbe79762dc2fa4e7c2421/`

## Answer

Every tool whose source is readable does the same thing: **poll `/proc/net/tcp` and `/proc/net/tcp6` for sockets in state `0A` (LISTEN), from a process running inside the environment's own network namespace, every 2 to 3 seconds, and diff against the previous scan.** None uses inotify, eBPF or netlink. VS Code adds a second, optional source: scanning terminal and debug-console output for `localhost`/`127.0.0.1`/`0.0.0.0` URLs.

| | VS Code (Remote-SSH, Dev Containers) | Codespaces | DevPod | Gitpod Classic | Ona (new Gitpod) |
|---|---|---|---|---|---|
| Mechanism | `/proc/net/tcp{,6}` + `/proc/*/fd` process match (default `process`); or terminal output (`output`); or both (`hybrid`) | Docs describe terminal output only; client is closed source | `/proc/net/tcp{,6}` | `/proc/net/tcp{,6}` | No auto-detection documented |
| Where the scan runs | VS Code server's extension host (VM for Remote-SSH, container for Dev Containers) | Undocumented | Helper inside the container | Supervisor inside the workspace | n/a |
| Interval | `max(20 × avg scan time, 2000 ms)` | Undocumented | 3 s | 2 s | n/a |
| Filter | Host must be loopback or all-interfaces; must map to a PID; excludes VS Code server's own processes; ports already open at connect are skipped | Undocumented | Only ports 1024 to 12000 | `ignore-completely` drops a port; unconfigured ports above 10000 get no notification | n/a |
| `127.0.0.1` vs `0.0.0.0` | Treated the same | Undocumented | Treated the same (no address filter) | Both detected; loopback-only gets an internal proxy | Must bind `0.0.0.0` |
| Port closes | `process`/`hybrid`: tunnel closed. `output`: stays forwarded | Undocumented | Forward stopped next scan | Not handled for auto-tunnels in the code read | n/a |
| Ignore setting | `portsAttributes` / `otherPortsAttributes` `onAutoForward: "ignore"`; `remote.autoForwardPorts: false` | `devcontainer.json` `portsAttributes` (labels documented) | None: `portsAttributes` is parsed but never read; whole feature off with IDE option `FORWARD_PORTS=false` | `.gitpod.yml` `ports[].onOpen: ignore-completely` | n/a |

## VS Code (Remote-SSH and Dev Containers)

VS Code's user docs ([Remote-SSH](https://code.visualstudio.com/docs/remote/ssh), [Dev Containers](https://code.visualstudio.com/docs/devcontainers/containers)) describe manual forwarding and `forwardPorts` only. They do not describe the auto-detection mechanism. Everything below comes from the source and the setting descriptions in `remote.contribution.ts`.

### Where the scan runs

- The scanner is `NodeExtHostTunnelService` in the **remote extension host**, i.e. the VS Code server process. It starts only when `isLinux && initData.remote.isRemote` ([`src/vs/workbench/api/node/extHostTunnelService.ts:194`](https://github.com/microsoft/vscode/blob/76e9dff43b237f3fcbb8a289ed2f8bdf28105baf/src/vs/workbench/api/node/extHostTunnelService.ts#L194-L197)).
- On a non-Linux remote, VS Code forces `remote.autoForwardPortsSource` to `output` ([`src/vs/workbench/contrib/remote/browser/remoteExplorer.ts:330-337`](https://github.com/microsoft/vscode/blob/76e9dff43b237f3fcbb8a289ed2f8bdf28105baf/src/vs/workbench/contrib/remote/browser/remoteExplorer.ts#L330-L337)). The setting description says the same: "On Windows and macOS remotes, the `process` and `hybrid` options have no effect and `output` will be used" (`remote.contribution.ts:234`).
- Because it reads `/proc/net/tcp` of its own process, it sees the **network namespace the VS Code server runs in**. With Dev Containers or "Attach to Running Container" that is the container. With Remote-SSH to a VM it is the VM's host namespace. A server inside a Docker container on that VM is then visible only if Docker publishes it on the host (the published `docker-proxy` listener is on the host). This is an inference from where the code reads, not something VS Code documents.

### The scan

`findCandidatePorts` ([`extHostTunnelService.ts:248-315`](https://github.com/microsoft/vscode/blob/76e9dff43b237f3fcbb8a289ed2f8bdf28105baf/src/vs/workbench/api/node/extHostTunnelService.ts#L248-L315)):

1. Reads `/proc/net/tcp` and `/proc/net/tcp6` and keeps rows with `st === '0A'` (LISTEN), keyed by inode, deduplicated on `ip:port` (`:49-64`, `:252-253`).
2. Maps socket inodes to PIDs by running `ls -l /proc/[0-9]*/fd/[0-9]* | grep socket:` (`:259-264`), and reads every `/proc/<pid>/cmdline` and `cwd` (`:266-283`).
3. Listeners whose inode maps to a PID become candidates with the command line as `detail`, except VS Code server's own processes (`.vscode-server-*/bin`, `out/server-main.js`, `_productName=VSCode`) (`knownExcludeCmdline`, `:108-115`; `findPorts`, `:133-148`).
4. Listeners whose inode maps to no readable PID (typically root-owned processes when the server is not root) go through a heuristic: run `ps -F -A -l | grep root` and pick the deepest child process whose command line contains the port number (`tryFindRootPorts`, `:150-179`; `:297-306`). If none matches, the candidate has no `detail`.

### Interval and filtering

- Loop: `$registerCandidateFinder` rescans forever while enabled; it only sends candidates to the UI when the list changed (`:218-235`).
- Delay: `Math.max(movingAverage * 20, 2000)` ms, where the moving average is the scan time, ignoring the first 4 scans (`:224-227`, `:238-241`). The code comment says scans typically take 50 to 100 ms, which gives the 2 s floor.
- Address filter: only candidates whose host is loopback (`localhost`, `127.0.0.1`, `::1`, `0:0:0:0:0:0:0:1`) or all-interfaces (`0.0.0.0`, `::`, `0:0:0:0:0:0:0:0`) are kept (`:220`; lists in [`src/vs/platform/tunnel/common/tunnel.ts:180-188`](https://github.com/microsoft/vscode/blob/76e9dff43b237f3fcbb8a289ed2f8bdf28105baf/src/vs/platform/tunnel/common/tunnel.ts#L180-L188)). A server bound to one specific interface IP (e.g. `172.17.0.2`) is never auto-forwarded.
- `127.0.0.1` vs `0.0.0.0`: both pass and are handled the same. The tunnel dials from the server side, so a loopback-only listener is reachable.
- `ProcAutomaticPortForwarding.forwardCandidates` ([`remoteExplorer.ts:741-794`](https://github.com/microsoft/vscode/blob/76e9dff43b237f3fcbb8a289ed2f8bdf28105baf/src/vs/workbench/contrib/remote/browser/remoteExplorer.ts#L741-L794)) then skips a candidate if:
  - it has no `detail` (no process found) (`:746-749`);
  - it is the local end of an existing remapped tunnel (`:750`);
  - it was **already listening when auto-forwarding started**, unless `portsAttributes` sets `onAutoForward` for it (`:762`; initial set captured in `setInitialCandidates`, `:724-739`);
  - it was already auto-forwarded or notified, or is already detected (`:765-769`);
  - its attributes say `onAutoForward: "ignore"` (`:773-776`).
- Fallback: with the default `process` source, once more than `remote.autoForwardPortsFallback` ports (default 20) have been auto-forwarded, VS Code switches the workspace to `hybrid` and shows "Over 20 ports have been automatically forwarded…" (`remoteExplorer.ts:255-310`; setting at `remote.contribution.ts:243-247`).

### Ports that close

- `process`: when a candidate disappears from the scan, `handleCandidateUpdate` closes the auto-forwarded tunnel with `TunnelCloseReason.AutoForwardEnd` and hides its notification ([`remoteExplorer.ts:796-841`](https://github.com/microsoft/vscode/blob/76e9dff43b237f3fcbb8a289ed2f8bdf28105baf/src/vs/workbench/contrib/remote/browser/remoteExplorer.ts#L796-L841)).
- `hybrid`: the process scanner runs with `unforwardOnly = true`. Detection comes from output, and the scanner only closes auto-forwarded tunnels whose port disappeared (`:343-345`, `:798-826`).
- `output`: "Ports forwarded based on output will not be 'un-forwarded' until reload or until the port is closed by the user in the Ports view" ([`src/vs/workbench/contrib/remote/common/remote.contribution.ts:238`](https://github.com/microsoft/vscode/blob/76e9dff43b237f3fcbb8a289ed2f8bdf28105baf/src/vs/workbench/contrib/remote/common/remote.contribution.ts#L232-L242)).

### Output parsing (`output` and `hybrid`)

[`src/vs/workbench/contrib/remote/browser/urlFinder.ts`](https://github.com/microsoft/vscode/blob/76e9dff43b237f3fcbb8a289ed2f8bdf28105baf/src/vs/workbench/contrib/remote/browser/urlFinder.ts#L17-L39) watches every integrated terminal and debug console:

- URL regex matching `localhost`, `127.0.0.1`, `0.0.0.0` or a bare `:port` (`:32`), plus a special case for Python's `Serving HTTP on 0.0.0.0 port 8000` (`:37`).
- Terminal data is debounced 500 ms (`:17`) and skipped when a batch exceeds 10,000 characters (`:24`).
- The terminal titled `Dev Containers` is excluded (`:39`).
- Matches with `onAutoForward: "ignore"` are skipped (`remoteExplorer.ts:623`).
- Output parsing misses anything that does not print a URL to a VS Code terminal (e.g. a daemon started by `postStartCommand`).

### Settings

Defined in [`remote.contribution.ts:222-363`](https://github.com/microsoft/vscode/blob/76e9dff43b237f3fcbb8a289ed2f8bdf28105baf/src/vs/workbench/contrib/remote/common/remote.contribution.ts#L222-L363):

- `remote.autoForwardPorts` (default `true`). Turning it off "will not prevent all ports from being forwarded": extensions and opened URLs can still forward.
- `remote.autoForwardPortsSource`: `process` (default), `output`, `hybrid`.
- `remote.autoForwardPortsFallback` (default 20; 0 disables).
- `remote.forwardOnOpen` (default `true`): forward when a local URL is opened from the terminal or debug console.
- `remote.portsAttributes`: keys are a port, a range (`"40000-55000"`), `"host:port"`, or a regex matched against the process command line (`".+\\/server.js"`). Attributes are `onAutoForward` (`notify` default, `openBrowser`, `openBrowserOnce`, `openPreview`, `silent`, `ignore`), `elevateIfNeeded`, `label`, `requireLocalPort`, `protocol`. Matching is in `PortsAttributes` ([`src/vs/workbench/services/remote/common/tunnelModel.ts:193-310`](https://github.com/microsoft/vscode/blob/76e9dff43b237f3fcbb8a289ed2f8bdf28105baf/src/vs/workbench/services/remote/common/tunnelModel.ts#L193-L310)).
- `remote.otherPortsAttributes`: same attributes for every port not matched above; `onAutoForward` here does not include `openBrowserOnce`.
- `remote.localPortHost`: `localhost` (default, binds `127.0.0.1` locally) or `allInterfaces` (`0.0.0.0`) (`extHostTunnelService.ts:317-320`).

Extensions can also contribute port attributes (`registerPortsAttributesProvider`, [`src/vs/workbench/api/common/extHostTunnelService.ts:106-118`](https://github.com/microsoft/vscode/blob/76e9dff43b237f3fcbb8a289ed2f8bdf28105baf/src/vs/workbench/api/common/extHostTunnelService.ts#L106-L118)). A remote resolver extension (Codespaces, Dev Containers, etc.) can override the detection source through the proposed `TunnelInformation.candidatePortSource` (`None`/`Process`/`Output`/`Hybrid`), and can filter candidates with `showCandidatePort` ([`src/vscode-dts/vscode.proposed.resolvers.d.ts:141-146`](https://github.com/microsoft/vscode/blob/76e9dff43b237f3fcbb8a289ed2f8bdf28105baf/src/vscode-dts/vscode.proposed.resolvers.d.ts#L141-L146), `:433`; applied in `src/vs/workbench/api/browser/mainThreadTunnelService.ts:220-240`, `extHostTunnelService.ts` (common) `:188-194`, `:289`).

## Dev container spec

[`docs/specs/devcontainerjson-reference.md`](https://github.com/devcontainers/spec/blob/c95ffeed1d059abfe9ffbe79762dc2fa4e7c2421/docs/specs/devcontainerjson-reference.md):

- `forwardPorts`: ports or `"host:port"` that "should always be forwarded from inside the primary container to the local machine". It is "most useful for forwarding ports that cannot be auto-forwarded because the related process that starts before the `devcontainer.json` supporting service / tool connects", which matches VS Code skipping ports already open at connect (line 12).
- `portsAttributes` / `otherPortsAttributes`: same keys and attributes as the VS Code settings (lines 13-14, 95-103). `onAutoForward: "ignore"` "means that this port should not be auto-forwarded at all" (line 101).
- The spec does not say how a tool should detect ports. Detection is left entirely to the implementing tool.
- `appPort` (line 47) publishes ports; it notes the app "may need to listen on all interfaces (`0.0.0.0`) not just `localhost`". VS Code's docs make the same point: "Forwarded ports, on the other hand, actually look like `localhost` to the application" ([Dev Containers docs](https://code.visualstudio.com/docs/devcontainers/containers)).
- Merge rule for image metadata: `otherPortsAttributes` last value wins; `forwardPorts` is a union (`docs/specs/devcontainer-reference.md:81-82`).
- Codespaces does not support the `"host:port"` form of `forwardPorts` or `portsAttributes` (`docs/specs/supporting-tools.md:153-154`).

## GitHub Codespaces

- The only documented detection is terminal output: "When an application running inside a codespace prints output to the terminal that contains a localhost URL, such as `http://localhost:PORT` or `http://127.0.0.1:PORT`, the port is automatically forwarded" ([Forwarding ports in your codespace](https://docs.github.com/en/codespaces/developing-in-a-codespace/forwarding-ports-in-your-codespace)).
- Defaults documented there: HTTP protocol, `private` visibility; `forwardPorts` in `devcontainer.json` for always-forwarded ports; labels written to `portsAttributes`.
- **Undocumented:** whether Codespaces also uses process (`/proc`) scanning, its poll interval, how it treats `0.0.0.0` vs `127.0.0.1`, and what happens when a port closes. The Codespaces VS Code extension and the codespace-side agent are closed source. When a codespace is opened in VS Code, VS Code's own auto-forward code above runs on the remote side; whether the Codespaces resolver overrides `candidatePortSource` cannot be checked from public source.

## DevPod

DevPod auto-detects ports in **one case only**: the browser-based VS Code IDE (`openvscode`). VS Code Desktop users get VS Code's own mechanism over DevPod's SSH connection.

- Opt-in path: IDE option `FORWARD_PORTS` ("If DevPod should automatically do port-forwarding", default `true`) exists only for `openvscode` ([`pkg/ide/openvscode/openvscode.go:31-48`](https://github.com/loft-sh/devpod/blob/5a0efcbff6610ab114b421f68a890739a452e66b/pkg/ide/openvscode/openvscode.go#L31-L48)), read at `cmd/up.go:777`. The Jupyter and RStudio browser tunnels pass `false` (`cmd/up.go:634`, `:691`). So does `devpod ssh` (`cmd/ssh.go:193-200`, `:550-556`).
- When enabled, the laptop starts `devpod agent container credentials-server --forward-ports` inside the container ([`pkg/tunnel/services.go:116-131`](https://github.com/loft-sh/devpod/blob/5a0efcbff6610ab114b421f68a890739a452e66b/pkg/tunnel/services.go#L116-L131)). That process runs a port watcher ([`cmd/agent/container/credentials_server.go:86-94`, `:193-195`](https://github.com/loft-sh/devpod/blob/5a0efcbff6610ab114b421f68a890739a452e66b/cmd/agent/container/credentials_server.go#L86-L94)). The scan therefore runs **inside the container's network namespace**.
- Watcher ([`pkg/netstat/watcher.go`](https://github.com/loft-sh/devpod/blob/5a0efcbff6610ab114b421f68a890739a452e66b/pkg/netstat/watcher.go#L32-L105)): every 3 s (`:37`), reads `/proc/net/tcp` and `/proc/net/tcp6` (`pkg/netstat/netstat_util.go:20-21`) for LISTEN (`0x0a`, `:40`) sockets. It keeps only ports in 1024 to 12000 (`watcher.go:97`). The code comment at `:94` says "that have a program assigned", but the code does no process lookup. There is **no address filter**, so `127.0.0.1`, `0.0.0.0` and specific-IP listeners are all forwarded.
- Each new port is sent over the tunnel's gRPC channel to the laptop (`ForwardPort`), which opens a laptop `localhost:<port>` listener and forwards it with SSH `direct-tcpip` to `localhost:<port>` on the container's SSH server ([`pkg/tunnel/forwarder.go:36-57`](https://github.com/loft-sh/devpod/blob/5a0efcbff6610ab114b421f68a890739a452e66b/pkg/tunnel/forwarder.go#L36-L57)). Ports already forwarded from `forwardPorts`/`appPort`, plus the openvscode port, are excluded (`isExcluded`, `:75-83`; list built at `pkg/tunnel/services.go:86-87`).
- Closing: ports missing from the next scan get `StopForward`, which cancels the forward (`watcher.go:52-61`, `forwarder.go:60-73`).
- Ignore settings: none. DevPod parses `portsAttributes` and `otherPortsAttributes` into its config, but no code reads them for forwarding. Its struct tag is `portAttributes`, not the spec's `portsAttributes` ([`pkg/devcontainer/config/config.go:58-63`](https://github.com/loft-sh/devpod/blob/5a0efcbff6610ab114b421f68a890739a452e66b/pkg/devcontainer/config/config.go#L58-L63)). The only off switch is `FORWARD_PORTS=false`.

## Gitpod Classic

Gitpod Classic (the `.gitpod.yml` product, now documented under Ona "classic") detects ports in the workspace supervisor.

- Poll: `PollingServedPortsObserver` with `RefreshInterval: 2 * time.Second` ([`components/supervisor/pkg/supervisor/supervisor.go:304-305`](https://github.com/gitpod-io/gitpod/blob/3e9ff3533fb6046fea96f0960896dffc7c66c972/components/supervisor/pkg/supervisor/supervisor.go#L304-L305)). It reads `/proc/net/tcp` and `/proc/net/tcp6` and keeps state `0A` ([`components/supervisor/pkg/ports/served-ports.go:45-46`, `:134`](https://github.com/gitpod-io/gitpod/blob/3e9ff3533fb6046fea96f0960896dffc7c66c972/components/supervisor/pkg/ports/served-ports.go#L127-L160)). Each port records `BoundToLocalhost: ipAddress.IsLoopback()` (`:152`).
- `127.0.0.1` vs `0.0.0.0`: if both exist for a port, the non-loopback entry wins ([`components/supervisor/pkg/ports/ports.go:282`](https://github.com/gitpod-io/gitpod/blob/3e9ff3533fb6046fea96f0960896dffc7c66c972/components/supervisor/pkg/ports/ports.go#L254-L306)). For a loopback-only listener, the supervisor starts a `tcpproxy` from `<workspace IP>:<port>` to `localhost:<port>` so the public port URL still works (`startLocalhostProxy`, `:852-867`; `updateProxies`, `:561-600`). Its own proxy listeners are ignored on the next scan (`:269-273`).
- Filtering: `.gitpod.yml` `ports[].onOpen: ignore-completely` drops the port before it is treated as served (`ports.go:276-279`). The schema says "'ignore-completely' will do nothing and prevent port forwarding" ([`components/gitpod-protocol/data/gitpod-schema.json:30`](https://github.com/gitpod-io/gitpod/blob/3e9ff3533fb6046fea96f0960896dffc7c66c972/components/gitpod-protocol/data/gitpod-schema.json#L20-L31)). `ignore` only suppresses the notification. With no config, ports above 10000 default to `ignore` and others to `notify_private` (`getOnOpenAction`, `ports.go:623-646`). Ranges are allowed ([Gitpod Classic ports docs](https://ona.com/docs/classic/user/configure/workspaces/ports.md)).
- Laptop forwarding: served ports are auto-tunnelled to connected clients with `TunnelVisiblity_host` (`autoTunnel`, `ports.go:521-559`). The docs say: "With VS Code Desktop, all ports are automatically forwarded", and the Local Companion can "automatically forward all ports from your workspace to localhost". The docs do not say which mechanism VS Code Desktop uses here.
- Ports that close: `autoTunnel` only adds tunnels for newly served ports. In the supervisor code read, nothing closes an auto-tunnel when its port stops listening. It is closed only when auto-tunnelling is disabled (`:522-531`), or replaced on an explicit `Tunnel` call (`:700-714`). The proxies, by contrast, are stopped when the port is no longer served (`:567-578`).

## Ona (current Gitpod)

- No auto-detection is documented. Ports are opened explicitly in the dashboard, the VS Code Ona view, or `ona environment port open 3000` ([Ona ports docs](https://ona.com/docs/ona/integrations/ports)).
- Services must "Listen on `0.0.0.0` (not `localhost` or `127.0.0.1`)". Tools that hardcode `localhost` return 503. The docs suggest iptables NAT or `socat` as workarounds.
- `forwardPorts` and `portsAttributes` "work in Ona across VS Code-based editors, but they operate independently from Ona's port sharing". Auto-forward notifications there come from the editor (VS Code's mechanism), not Ona.

## Implications for devcrate

- Every implementation polls `/proc/net/tcp{,6}` from inside the namespace where the servers run. For devcrate that is the devcontainer's network namespace, which the Agent already enters per [ADR 0003](https://github.com/ianmurrays/devcrate/blob/main/docs/adr/0003-ports-are-dialed-from-inside-the-container.md). The Agent can read `/proc/<container pid>/net/tcp` from the VM without entering the namespace, because `/proc/<pid>/net` reflects that process's namespace (Linux `proc(5)`; not verified against a running devcrate).
- Dialing from inside the namespace makes `127.0.0.1` and `0.0.0.0` equivalent, as in VS Code and DevPod. Gitpod needed an extra proxy only because it exposes ports via the workspace IP.
- The #10 resolution says VS Code users get auto-detection "from VS Code's own Remote-SSH forwarding". That holds only where the VS Code server runs. Under plain Remote-SSH to the VM, VS Code scans the VM's namespace and will not see unpublished ports inside the devcontainer. Under "Attach to Running Container" it scans the container and does see them.
- Reasonable ignore semantics, if auto-detect is added: honour `portsAttributes` / `otherPortsAttributes` `onAutoForward: "ignore"` (spec-defined), skip ports already open when `forward` starts unless listed (VS Code behaviour, and why `forwardPorts` exists), and stop forwarding when a port disappears from a scan (VS Code `process` mode and DevPod both do this after one missed scan).
