# Fleet-man Feature Exploration

This document designs two composed features for fleet-man: (1) pointing a fleet at an existing folder as its project root via a live bind-mount, and (2) making fleet usable when developing on a remote machine over SSH. Per the hard maintainer constraint, both features assume the devcontainer, its folders, and fleetd all live and run **on the remote box, in place** — never copied, moved, or relocated under `~/.fleet/workspaces`. The two features fuse into one real workflow: *from my laptop, point fleet at existing folders on my remote dev box and run their devcontainers in place on that box.*

The good news, verified against the code: most of the machinery already exists. Feature 1's live bind-mount is nearly free because `devcontainer up --workspace-folder` already bind-mounts read-write; Feature 2's interactive exec, browser, and Fleet Launch already tunnel to the laptop. The real work is field-threading and a handful of safety gates.

---

## 0. Decisions & scope (locked)

Maintainer decisions taken after the initial exploration:

1. **One instance per local-folder fleet** — hard-error on a second (sidesteps the `devcontainer.local_folder` container-label collision and the undefined shared-tree branch semantics).
2. **Drop `fleet code` remote** — the location-aware `ResolveEditor` RPC and `editor_ssh_host` config are **out of scope**. VS Code, if wanted, is reached out-of-band via laptop **Remote-SSH** into the box.
3. **Skip `editor_ssh_host`** — follows from (2).
4. **Target is Topology A** (fleet TUI on the laptop, daemon + Docker + folders on the remote box). Topology B (running the whole TUI on the remote over SSH) is **out of scope** — see §0.2.

### 0.1 How the two features relate (dependency)

They are **orthogonal**, but the maintainer's real workflow needs both:

- The **remote feature (Topology A)** does **not** depend on Feature 1. Driving a remote daemon works *today* with a git remote: `fleet up agent-1 --repo git@…` against a remote daemon clones into `~/.fleet/workspaces` **on the remote box**.
- The goal — *"point at an existing folder on the remote and run its devcontainer in place"* — **does** need Feature 1, because the git-remote path clones into an isolated dir rather than mounting an existing folder in place.
- **`--path` resolves on the DAEMON HOST (the remote box), not the laptop.** `create.Run` runs on the daemon, so the path string is interpreted against the remote filesystem — no clone, no upload from the laptop. This is precisely the in-place behavior wanted. Consequence: in v1 the user types an absolute *remote* path (no laptop→remote path tab-completion).

### 0.3 Connecting laptop → remote daemon (security) — CONFIRMED against the code

How the laptop reaches the remote daemon matters, and the code makes the choice for us:

- **`FLEET_SERVER` (plain TCP) is unsafe on any shared/untrusted network.** Confirmed: the client attaches **no** bearer token on this path (`fleetclient/endpoint.go:73` — `remoteEndpoint.DialOptions()` returns only `insecureCreds()`, unlike the gateway path's `WithPerRPCCredentials` at `grpc_gateway.go:113`). And the daemon exposes **no authenticated TCP surface**: `server.go` binds the `FleetService` only on (a) the **unix socket** with `grpc.NewServer()` and **no interceptor** — auth-less by design, gated by 0600/same-user (`remote_auth.go:15-16`), and (b) the **gateway tunnel** on an in-memory `ChanListener`, which is the *only* surface wrapped in `bearerAuthInterceptors` (`server.go:180-186`). There is no `net.Listen("tcp", …)` for the FleetService. So `FLEET_SERVER` can only reach the daemon if the user bridges the auth-less unix socket to a TCP port themselves — which then grants **full, unauthenticated daemon control** to anyone who can reach it.
- **`FLEET_GATEWAY` is the token-authenticated remote path** (bearer token enforced end-to-end). The confirmed browser proxy (§0.2) rides `FleetService`, so it works over the gateway. Cost: running a gateway relay.
- **SSH-forwarding the daemon's unix socket** is the best fit for the "develop over SSH" setup: authenticated + encrypted by SSH, no public relay, and the daemon stays same-user/auth-less as designed. Gap: the client has **no way to target an arbitrary socket path** today (local endpoint is hard-wired to `fleetpaths.SocketPath()`; only `FLEET_SERVER`/`FLEET_GATEWAY` override it). A small enhancement — a `FLEET_SOCKET=/path` endpoint (or an `ssh://user@host` endpoint that sets up the forward) — would make this a first-class, secure option. **Candidate follow-up, not in the current plan.**

### 0.2 Browser (`b`) — CONFIRMED against the code

Pressing `b` with the TUI on the laptop and the daemon remote opens **Chrome on the laptop**, proxied **into the remote container** (so it reaches `localhost:3000` etc. inside that container), with **zero code changes**. Traced end-to-end:

- `tui/page_fleet_keys.go:430` (`b` → `beginBrowserOpen`) → `tui/browser.go:58` `openBrowserProxyCmd`.
- **`PrepareBrowser` RPC** → `server/browser.go:87` starts **privoxy** (port 58888) inside the container on demand.
- **`forwardDialer`** (`tui/exec_client.go:159`) → `portforward.NewGRPCTarget` (`portforward/grpc_target.go:19`) opens a bidirectional **gRPC `Forward` stream** over the *same* connection the TUI already holds; `server/forward.go:71` bridges it to the container's privoxy (direct TCP, else `ForwardStdioCommand`).
- **`launchBrowser`** (`tui/browser.go:239`) runs `exec.Command("google-chrome", "--proxy-server=http://localhost:<port>", …)` **on the TUI host = the laptop**.

The whole path rides `FleetService`, which is transport-agnostic (unix socket ↔ gateway/`FLEET_SERVER` TLS-TCP are identical to it). Prereqs: privoxy installable in the container (daemon handles it), and a Chromium-family browser on the laptop. In Topology B the proxy still sets up but `launchBrowser` finds no browser on the headless host and fails — which is why the target is Topology A.

---

## 1. Feature 1 — Local folder as project root (live bind-mount)

### 1.1 Context: today's git-remote model

A fleet's *identity and source are the same thing*: it is keyed by `Name` in `state.Fleets` and its source of truth is `Fleet.Remote` (a git remote URL). The name is **derived** from that URL (`FleetNameFromRemote` strips host/org and `.git`). Provisioning flow:

1. `internal/cli/up.go` resolves the target (`fleet.Resolve`) and the remote URL (`--repo` > fleet record's `Remote` > `RemoteURLFromCwd`), then sends a `CreateInstanceRequest{fleet, instance, remote, backend, branch}`.
2. The **server** is authoritative: `startCreateInstanceJob` (`internal/server/jobs.go:443`) runs `GetOrCreateFleet(name, remote)` — which **hard-errors if a new fleet has no remote** (jobs.go:459-461) — then `AddInstance` with `WorkspaceDir = ~/.fleet/workspaces/<fleet>/<instance>/<fleet>`, `Status=Creating`.
3. `create.Run` (`internal/create/create.go`) does `git clone <remote> <wsDir>` (lines 57-80) then `backend.Up(wsDir, mounts)`.
4. The devcontainer backend runs `devcontainer up --workspace-folder <wsDir>`, which bind-mounts that folder read-write as the project root.

So **the workspace is already a live bind-mount — of a private clone.** Feature 1 is mostly substituting `wsDir` for the user's absolute path and skipping the clone. The mount mechanism is unchanged.

### 1.2 The design

Introduce a **`SourceKind`** (`git-remote` | `local-folder`) and **`SourcePath`** on the data model. A git-remote fleet keeps today's behavior. A local-folder fleet stores the absolute daemon-host path and uses it directly as the `--workspace-folder`, skipping the clone entirely. The folder's own `.devcontainer/devcontainer.json` is honored unchanged; fleet synthesizes nothing.

Everything downstream of `backend.Up` (control mount, buildkit, deb/image caches, dotfiles, symlinks) keeps working **without change**, because those all live under `~/.fleet` as siblings of the workspace (e.g. `.control` at `WorkspacesDir()/<fleet>/<instance>/.control`) and are keyed by fleet/instance, not by the workspace tree.

**Decision — one instance per local-folder fleet (hard-error on a second).** The `devcontainer.local_folder=<path>` label is the container identity key; `pruneStaleContainers` force-removes any container with a matching label. Two instances on the same in-place path would fight over one container, and a shared working tree defeats per-instance branch isolation. Restricting to one instance sidesteps the label collision *and* the undefined branch semantics.

### 1.3 Data-model & CLI changes

| File | Change |
|---|---|
| `internal/fleet/instance.go` | Add `SourcePath string` and `SourceKind` (json `omitempty`; empty == git-remote for legacy round-trip). |
| `internal/fleet/fleet.go` | Add `SourceKind` (and optionally `SourcePath`) for identity/dedup; `Remote` stays empty for local-folder fleets. |
| `internal/fleet/fleet_name_from_remote.go` | Add `FleetNameFromPath(absPath)` = `filepath.Base` (the sole naming fn today is remote-only). |
| `internal/fleet/resolve.go` | Local-folder naming branch (basename or explicit `fleet/instance`); do **not** run cwd git-remote inference in this mode. |
| `internal/cli/up.go` | Add `--path/--dir` (mutually exclusive with `--repo`; reject `--branch`). Resolve to absolute, skip remoteURL resolution (41-54), set `SourcePath` + `SourceKind=LOCAL_FOLDER`. |
| `fleetgrpc/jobs.proto` | Add `optional string source_path` (tag 7) + `SourceKind source_kind` (tag 8) to `CreateInstanceRequest`. |
| `fleetgrpc/domain.proto` | Add `SourceKind` enum; add `source_path`/`source_kind` to proto `Instance` from reserved 14+ (optionally `Fleet`). |
| `internal/protoconv/protoconv.go` | Map both fields in **both** directions of `InstanceToProto/FromProto` (and Fleet mappers) — mapped by hand; a missed field silently drops over the gateway/tunnel. |
| `internal/state/state.go` | Add `FindFleetBySourcePath` (normalized abs path); relax `GetOrCreateFleet` to allow an empty remote for local-folder fleets. |
| `internal/server/jobs.go` | `startCreateInstanceJob`: when `LOCAL_FOLDER`, skip the no-remote error (459-461), set `wsDir=source_path`, persist source fields, enforce one-instance rule. Thread source through `jobRunCreate → create.Run`. **Teardown (791-799): the data-loss fix — see below.** |
| `internal/create/create.go` | `Run`: accept source kind/path; when local-folder set `wsDir=sourcePath` and skip `MkdirAll`+`git clone` (57-80). |
| `internal/backend/devcontainer/devcontainer_backend.go` | `EditorURI` (688-693): derive `projectName` from `basename(workspaceDir)`/`RemoteWorkspaceFolder`, not the fleet name (a folder's basename may differ from the fleet name). |
| `internal/cli/code.go` | Same `projectName` derivation for the client-inlined dev-container URI. |
| `internal/backend/devcontainer/conflicting_mounts.go` | For local-folder sources, make the restore crash-safe or skip in-place mutation (see risk below). |

New user-facing behavior: `fleet up <name> --path <abs-dir>` provisions in place, edits flow both ways in real time, the folder's own devcontainer.json is honored, `--path` excludes `--repo`/`--branch`, only one instance per local folder, `fleet destroy` removes the container but **never** the folder, and the path resolves on the **daemon host** (must be absolute).

### 1.4 The isolation tension & how it's handled

Fleet's whole value-prop is bind-mounting a **private clone** to isolate instances. A shared live bind-mount means the agent's edits and any `postCreate`/`npm install` side effects mutate the user's **real** working tree in real time — the opposite of the clone-isolation default. This is intentional in live-share mode but must be **surfaced explicitly**: a warning on `up`, and honest docs. We contain the damage by (a) restricting to one instance per folder (no cross-instance contention on the index), (b) rejecting `--branch` (a live tree cannot be re-pointed without disturbing uncommitted work), and (c) disallowing `Clone`/leaving `Rebuild` to preserve the in-place mount.

### 1.5 Risks

| Risk | Mitigation |
|---|---|
| **Teardown `os.RemoveAll(workspaceDir)` (jobs.go:~796) deletes the user's real folder — catastrophic data loss.** | Snapshot `SourceKind` into the teardown target struct (785) and **skip `RemoveAll` entirely** for `LOCAL_FOLDER` (only `docker rm`). Add a test asserting the folder survives destroy. **Must-not-ship-without.** |
| Two instances on one folder fight over one container (label collision). | Enforce one-instance-per-local-folder-fleet in `startCreateInstanceJob`; document. |
| `neutralizeConflictingMounts` rewrites the user's *tracked* devcontainer.json during `up`; a mid-`up` crash leaves it rewritten (comments/formatting lost). | For local-folder sources, skip in-place mutation (hard-error on mount conflict) or make restore crash-safe via an atomic backup. |
| New field dropped across gateway/tunnel (protoconv maps by hand). | Map in both directions; cover with a round-trip test. |
| Cross-UID friction: container-user writes land on the host folder as the container user; `--update-remote-user-uid-default` may adjust the container user. The resolver's 0777/0666 tricks don't apply to the user's folder. | Document; optionally warn. Out of scope to fully solve for live-share. |
| Basename collision between two different folders. | Source-path-aware dedup (`FindFleetBySourcePath`); require explicit `fleet/instance` on collision. |

### 1.6 Effort

**M.** The mount itself is essentially free; the bulk is boilerplate field-threading (struct → proto → protoconv → server → create.Run) plus two must-not-ship-without safety items (teardown gate, devcontainer.json mutation). The single product decision (one vs N instances) is resolved by restricting to one.

### 1.7 Open questions

- Model source on `Instance` only, or also `Fleet`? (Instance suffices for create/teardown; Fleet is cleaner for dedup — recommend both.)
- Should the daemon validate `source_path` exists and contains a devcontainer.json before pre-creating the record, to fail fast?
- For remote daemons, is a raw absolute path acceptable for v1, or do we want daemon-host path tab-completion from the client?
- `Clone` of a local-folder instance: disallow (recommended) or clone the live tree into an isolated copy?

---

## 2. Feature 2 — Remote-SSH development

Two topologies. Judged by the maintainer constraint: does it support *daemon + Docker + folders all on the remote, in place, driven from the laptop*? A topology requiring the folder to be copied or Docker to run on the laptop is disqualified. **Both topologies keep Docker and the folders on the remote**, so both are admissible; they differ in *where the fleet client/TUI runs*.

### 2A. Topology A — daemon on remote (gateway hardening)

Docker + fleetd run on the remote; the laptop drives them via the existing `FLEET_GATEWAY` reverse-tunnel gRPC path (`internal/gateway`, `internal/tunnel`, `internal/fleetclient`). This composes **natively** with Feature 1: `create.Run` runs on the remote, so an in-place bind-mount of a remote folder Just Works. This is hardening, not building — most of it exists.

**Interactive exec — ~95% reuse.** The PTY-over-tunnel path is complete: `exec.proto` Exec bidi, server handler `internal/server/exec_stream.go` (`creack/pty.Start`, `pty.Setsize` on resize, `killOnDisconnect`), client driver `internal/execstream`, and the gateway's transparent proxy (`UnknownServiceHandler` proxies any bidi RPC by `fleet-session`). `fleet shell` already branches on `IsRemote()` (`shell.go:81-86`). The gap is that `internal/cli/exec.go` **never** branches — it unconditionally calls `runResolvedExec`, so `fleet exec` against a remote daemon runs the daemon-host argv on the laptop. **`doc/gateway.md` §10 is stale** — it claims the server Exec handler is unbuilt.

Fix: mirror `shell.go` in `exec.go`, plus TTY-vs-pipe detection via `term.IsTerminal(stdin)` so scripted `fleet exec` keeps `stderr` separate (server's `runExecPipes`) and interactive gets a PTY (`runExecTTY`).

**Browser + Fleet Launch — CONFIRMED 100% reuse for A (zero code).** Traced end-to-end in §0.2. The privoxy-over-`Forward` proxy (`tui/browser.go` + `forwardDialer` in `tui/exec_client.go`) tunnels the container proxy to the laptop over the gRPC `Forward` stream; Chrome launches where the TUI runs (the laptop). Fleet Launch open-on-host rides the **daemon-co-located** control socket (`server/control.go`) → `hub.broadcastBrowserOpen` → Watch `Event_BrowserOpen` (`tui/watch.go:211`) → `tui/app.go:845` → local proxied Chrome. "Control-socket-over-tunnel" is a **non-goal** — the socket correctly stays next to the container; only the open-intent event crosses to the laptop, which it already does over the Watch stream. Clipboard works because the TUI is on the laptop.

**`fleet code` remote — DROPPED (maintainer decision #2).** `code.go`/`EditorURI` encode a remote `WorkspaceDir` into a dev-container URI that local `code` resolves against *local* Docker, which is nonsensical remotely — but making it location-aware requires a `ResolveEditor` RPC **and** teaching fleet the daemon's SSH-reachable hostname (a concept it has nowhere today). That is out of scope. Users who want VS Code reach the remote container **out-of-band**: open VS Code on the laptop, **Remote-SSH** into the box, then Dev Containers "Reopen in Container" — no fleet changes. This costs nothing and sidesteps the one genuinely hard, config-heavy piece.

The remaining A work is therefore just the **interactive-exec wiring** plus a doc fix:

| File | Change |
|---|---|
| `internal/cli/exec.go` | Add `if IsRemote()` branch (mirror `shell.go:81-86`); pick TTY vs pipe via `term.IsTerminal(stdin)`. |
| `internal/cli/exec_client.go` | Add `runRemoteExecPiped` (or a `tty bool` on `runRemoteShell`) calling `execstream.Run(TTY=false)`. |
| `doc/gateway.md` | Correct §10 (the server Exec handler exists and is already used by `fleet shell`). |

**Risks:** regressed pipe semantics if exec always uses a TTY (gate on `term.IsTerminal`); version skew with an old fleetd (surface via `mapRPCError` → upgrade hint). The `fleet code` / `editor_ssh_host` risks no longer apply.

**Effort: S** — browser/Fleet Launch/clipboard are confirmed zero-code (§0.2); the exec branch is ~a few lines of reuse.

### 2B. Topology B — fleet on remote via SSH (GUI forwarding)

The user SSHes into the remote and runs the **entire** stack there — fleetd, Docker, **and** the TUI/CLI. Endpoint resolution falls through to the local unix socket, so `IsRemote()==false` and the gateway/tunnel is bypassed entirely.

**Layer 1 — zero fleet changes:**
- **Interactive terminal** (`exec`/`shell`/`launch`/tmux): fully works — client host == daemon host, so the TTY carve-out (`ResolveExecCommand` → local `docker`/`devcontainer exec`) is correct. B's strongest feature.
- **VS Code**: open VS Code on the laptop, Remote-SSH into the box, Dev Containers "Reopen in Container." The existing `vscode-remote://dev-container+<hex(WorkspaceDir)>` URI is now **correct** because Docker is local to the Remote-SSH server.
- **App ports**: plain `ssh -L`.

**Layer 2 — the real gap (GUI launchers on a headless host).** `browser.go` `launchBrowser` (239), `openurl.go` `openExternalURL` (49/65), and `cli/code.go` each exec a GUI binary on the TUI's own host — the **headless remote**. The full control-socket → hub → Watch → `app.go` → `launchBrowser` chain runs and lands on the wrong host; the built-in browser and Fleet Launch open-on-host **fire into the void with no error**, and clipboard silently no-ops (`newClipboardSync` returns nil).

| File | Change |
|---|---|
| `internal/tui/browser.go` | `launchBrowser`: headless branch (no `DISPLAY`/`WAYLAND_DISPLAY`, not WSL/darwin, or `$FLEET_BROWSER_OPENER`/`$BROWSER` set) → don't exec Chrome; invoke opener or print proxied URL + port. |
| `internal/tui/openurl.go` | `$BROWSER` override + headless fallback (OSC 8 hyperlink / plain print); reuse `isBrowsableURL` http(s) guard. |
| `internal/cli/code.go` | `--print/--uri` (or headless auto-detect) → emit the dev-container URI + Remote-SSH instructions instead of exec'ing local `code`. |
| `internal/tui/clipboard.go` | Force the OSC 52 path (already emitted in `page_settings.go:835`) when headless; surface a hint that clipboard needs an OSC-52-capable terminal. |
| `internal/server/browser.go` | Optional: a mode where `PrepareBrowser` returns URL + privoxy port without launching, so a laptop-side helper over `ssh -L` opens it. |

**The honest limit:** unlike A (where `forwardDialer` tunnels privoxy over gRPC and the TUI is on the laptop), B has **no reverse channel** back to the laptop GUI. The escape hatches are a printed/clickable URL (terminal-dependent) or manual `ssh -L` to daemon-side privoxy + laptop Chrome (works, not automatic). Clipboard is only partially solvable: OSC 52 works for capable terminals (Kitty/Alacritty/iTerm2/WezTerm); the tmux-polling fallback (GNOME Terminal/Ptyxis) is **un-bridgeable** over SSH.

**Risks:** silent-failure UX (open-on-host fires into the void); forwarding an in-container-requested `browser.open` URL to the laptop opener widens the attack surface (reuse the http(s)-only guard); opening laptop VS Code *without* Remote-SSH first resolves the URI against local Docker and fails (`fleet code` output must instruct Remote-SSH-first); the headless heuristic false-positives on VNC'd boxes.

**Effort: M**, mostly zero-code value (shell + VS Code) plus a small, localized, but **throwaway-for-GUI** hardening layer.

### 2C. Feasibility comparison & recommended sequencing

| Dimension | Topology A (daemon remote, TUI on laptop) | Topology B (whole stack on remote) |
|---|---|---|
| Maintainer constraint (in-place remote folder) | Native — `create.Run` runs on the remote | Native — same |
| Interactive shell/exec | ~3-line wiring (mostly reuse) | Works with **zero** code |
| VS Code | Net-new `ResolveEditor` + `editor_ssh_host` | Works via laptop Remote-SSH, **zero** code |
| Built-in browser / Fleet Launch | Works with **zero** code (TUI on laptop) | Structurally broken (headless), only awkward bridges |
| Clipboard | Works (TUI on laptop) | OSC 52 only; tmux fallback un-bridgeable |
| Trust surface | Gateway sees plaintext token; needs token/gateway setup | Plain SSH the user already has |
| Setup cost | Higher (gateway/`FLEET_SERVER` + token) | Near-zero |
| Hardest part | Daemon SSH-host concept for `fleet code` | Browser/Fleet Launch with no reverse channel |
| Long-term | **Strategic destination** — full GUI parity | Tactical; collapses into A for GUI-heavy work |

They **coexist**, selected by endpoint: B is the local-socket path (`IsRemote()==false`), A is the gateway/`FLEET_SERVER` path. Both go through the same `FleetService`.

**Decision: build Topology A; Topology B is out of scope.** The deciding factor is the built-in browser (`b`): it works exactly as wanted only when the TUI is on the laptop (§0.2), and is structurally broken on a headless remote (B). A also satisfies the in-place-remote-folder constraint natively — `create.Run` runs on the remote, so Feature 1's in-place mount Just Works — and makes browser + Fleet Launch + clipboard work with **no** code. With `fleet code` dropped, A's entire remaining cost is the small interactive-exec wiring. B's throwaway GUI bridges (`ssh -R` to privoxy, tmux-clipboard-over-SSH) are explicitly **not** pursued. (If someone SSHes into the box and runs fleet there anyway, interactive exec/shell/launch/tmux already work with zero changes — that path is simply undocumented, not built-for.)

---

## 3. Recommended overall plan (phased)

Target: **Topology A + Feature 1**. `fleet code` remote and Topology-B GUI hardening are dropped.

| Phase | Scope | Effort | Notes |
|---|---|---|---|
| **P0.5 — SSH secure transport (`FLEET_SOCKET` + `FLEET_SSH`)** ✅ DONE | `FLEET_SOCKET=/path` (`socketEndpoint`) for a hand-forwarded socket, **and** `FLEET_SSH=ssh://[user@]host[:port][/sock]` which sets the forward up automatically via the system `ssh` + an SSH ControlMaster (`fleetclient/ssh_tunnel.go`) — supports key / password / agent auth, one-time authentication reused across commands, auto-resolves the remote socket path. Both counted as remote by `IsRemote()`; armada switcher fully wired (`armada.go`, `app.go`); unit tests in `fleetclient/socket_endpoint_test.go`, `fleetclient/ssh_tunnel_test.go`, `tui/armada_test.go`. Build + vet + unit tests + depguard lint all green. | **S–M** | Lets the laptop drive the remote daemon over SSH — authenticated by SSH, no token, no gateway. See §0.3 + "Usage" below. |
| **P0.6 — SSH remote in the TUI (Armada registration)** ✅ DONE | An SSH remote is registered as an **Armada member** (not a fleet): Settings → Fleet Armada → **"+ SSH Remote"** (single `ssh://` field, no token; connection test = establish tunnel + Hello). Discriminated from gateway remotes by URL scheme (`ssh://`) so no proto/registry-schema change is needed; persisted to `~/.fleet/armada.json`; selectable from the `A` armada selector (`switchArmada` sets `FLEET_SSH`). Recurring status sweep skips ssh remotes (would re-auth a tunnel per tick); they test on add + on-demand (`enter`). `pingArmadaRemote` branches on scheme → `fleetclient.PingSSH`. Files: `tui/page_settings.go`, `tui/armada.go`, `tui/armada_client.go`, `fleetclient/ping.go`. Key/agent auth first (password via CLI/ControlMaster, per decision). Build + vet + tests + lint green. | **M** | The user-facing "create a new remote connection" UX. |
| **P1 — Feature 1 (local folder / in-place live bind-mount)** | ✅ DONE (one nit deferred). **`fleet up <name> --path <abs-dir>`** creates a local-folder instance: the folder is bind-mounted IN PLACE (no clone/copy), edits flow both ways. Implemented: teardown data-loss gate (`fleetpaths.IsManagedWorkspace`, gates `destroy()` `os.RemoveAll`); `SourcePath` on the fleet/instance model + `CreateInstanceRequest.source_path` (real `buf` proto regen — only the new field changed); `create.Run` skips clone & uses the folder as the workspace; server relaxes the no-remote guard for local folders, sets `WorkspaceDir=source_path`, persists source, and enforces **one instance per local-folder fleet** (+ guards against mixing source kinds); `--path` is mutually exclusive with `--repo`/`--branch` and must be absolute for a remote daemon; devcontainer.json mutation is skipped for in-place workspaces (`neutralizeConflictingMounts` no-ops when not managed). Tests: `fleetpaths`/`state` invariant, `fleet.ResolvePath`/`FleetNameFromPath`, server `TestCreateInstanceLocalFolder` (no-remote, in-place workspace, source persisted, one-instance rule); updated the devcontainer neutralize tests to use a managed workspace. Build + vet + tests + gofmt + depguard lint all green. **Deferred nit:** `EditorURI` projectName still derives from the fleet name — harmless in the common case (fleet name == folder basename) and only cosmetic for local `fleet code` under an explicit `fleet/instance` name. | **M** | Composes with Topology A automatically (daemon runs where the folder is). |
| **P2 — `fleet exec` remote wiring** | `IsRemote()` branch in `cli/exec.go` (mirror `shell.go`) + TTY/pipe detection via `term.IsTerminal`; reuse `execstream`. | **S** | ~a few lines + a helper; closes the documented exec gap. Browser/Fleet Launch/clipboard already work (§0.2). |
| **P0 — Docs** | Document the Topology-A workflow (TUI on laptop → remote daemon via gateway/`FLEET_SERVER` → `fleet up --path <remote-abs-path>`; `b` opens a laptop browser into the remote container). Correct stale `doc/gateway.md` §10. | **XS** | Can land alongside P1/P2. |

This is a small, low-risk plan: one **M** feature (mostly boilerplate + two safety gates) and one **S** wiring fix. The one blocking item is the P1 teardown gate (data-loss).

### P1.1 — TUI "new fleet from local folder" ✅ DONE

The `n` (new-fleet) dialog now accepts an **absolute path** (auto-detected) as
well as a git URL: an absolute path registers a local-folder fleet (empty remote,
`SourcePath` set) directly, skipping the clone-inspect; `a` then creates its
single instance. Wiring: `CreateFleetRequest.source_path` (proto regen — only the
new field), `mutations.go` persists it, `createFleetRemote` gained a `sourcePath`
arg, `saveAddFleet`/`addLocalFolderFleet` handle the folder branch. Crucially the
server now **infers `source_path` from the fleet record** when a `CreateInstance`
request omits it, so the TUI `a` flow (and `fleet up <name>` against an existing
local-folder fleet) provision in place with no client changes. Tests:
`tui.TestSaveAddFleetLocalFolder` / `TestSaveAddFleetRemoteStillInspects`,
`server.TestCreateInstanceInheritsFleetSourcePath`. Build/vet/tests/gofmt/lint green.

### 3.1 Usage — driving a remote daemon over SSH (P0.5)

Easy path — fleet sets the tunnel up for you:

```bash
# Auto-resolves the remote ~/.fleet/fleet.sock and forwards it. Auth = whatever
# ssh does (key / password / agent); authenticate once, reused across commands.
FLEET_SSH=ssh://dev@remote-box fleet
# Non-default user/port/socket: FLEET_SSH=ssh://dev@remote-box:2222/home/dev/.fleet/fleet.sock
```

Manual path — own the `ssh -L` yourself:

```bash
ssh -N -f -o StreamLocalBindUnlink=yes \
  -L /tmp/fleet-remote.sock:$HOME/.fleet/fleet.sock user@remote-box
FLEET_SOCKET=/tmp/fleet-remote.sock fleet
```

Notes:
- **Prerequisite: `fleetd` must already be running on the remote** (plus `fleet` + Docker + devcontainer CLI installed there). The SSH paths connect to an existing daemon and its `~/.fleet/fleet.sock`; they do **not** auto-start it (auto-spawn is local-only by design — a client can't fork-exec across SSH). Simplest: SSH in once and run `fleet ls` (auto-spawns the daemon there). Decision: no remote auto-start — keep it explicit, no assumptions about the remote's non-interactive `PATH`.
- No token needed — SSH authenticates the transport; the daemon socket is auth-less by design (§0.3).
- `FLEET_SSH` uses an SSH ControlMaster (`ControlPersist=300`), so a password/passphrase is asked once and later `fleet` commands reuse the connection. For password auth with the TUI, run one CLI command first so the prompt isn't drawn over the full-screen UI.
- Implementation drives the system `ssh` binary (`fleetclient/ssh_tunnel.go`) — inherits `~/.ssh/config`, `known_hosts`, `ProxyJump`. Requires OpenSSH ≥ 6.7 (unix-socket forwarding).
- Precedence: `FLEET_GATEWAY` > `FLEET_SERVER` > `FLEET_SOCKET` > `FLEET_SSH` > local socket.

---

## 4. Open questions for the maintainer

Resolved by decision: one-instance-per-folder (yes), `fleet code` remote (dropped), `editor_ssh_host` (dropped), Topology B GUI hardening (dropped).

Still open:

1. **Feature 1 model.** Source on `Instance` only, or also `Fleet`? (Recommend both: Instance for create/teardown, Fleet for clean dedup.)
2. **`--branch` with `--path`.** Hard error (recommended), or a soft in-place `git checkout` (risks uncommitted work)?
3. **Daemon-side path validation.** Should the daemon validate `source_path` exists + has a `devcontainer.json` before pre-creating the record, to fail fast? (Recommended — the path is on the remote host, so a clear early error beats a mid-provision failure.)
4. **`fleet exec` TTY selection.** Auto-detect via `term.IsTerminal` (recommended), or add an explicit docker-style `-t/--tty` flag?
5. **`FLEET_SERVER` (plain TCP) auth — RESOLVED (confirmed unsafe; see §0.3).** The client sends no token on this path and the daemon has no authenticated TCP surface (only the auth-less unix socket + the token-gated gateway tunnel). Do **not** expose `FLEET_SERVER` on a shared network. Open sub-question: adopt a first-class secure SSH path — a `FLEET_SOCKET=/path` (or `ssh://user@host`) endpoint so the laptop can drive an SSH-forwarded unix socket without a gateway?
6. **Live smoke test.** Findings are static; confirm a resize + full-screen app (vim/htop) over the gateway's yamux chunking and a piped `fleet exec` (stderr separation) end-to-end, plus `b` → laptop browser → remote container against a real remote daemon.