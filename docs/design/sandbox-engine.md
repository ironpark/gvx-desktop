# Sandbox engine, phase 1 (experimental)

Status: design and compiling skeleton. Branch `feat/sandbox-engine`.
Requirements: the coordinator's `sandbox-engine-requirements.md`. Its SAFETY
section binds every workstream. In short: never touch the user's running sandboxes, the app's real data
dir, `~/.gvx/docker.sock`, `~/.gvx/engine/`, `~/.gvx/k3s/`, docker contexts,
`~/.docker` (reading the compose plugin link is fine), `~/.kube`, or the real
rungv cache. Test containers are named `gvxd-*`. Use only ports 18090-18099
and 15xxx. Never launch the app in normal mode. Kill only what you started.

## 1. What it is

Today `docker` reaches a dockerd that runs inside the "Docker Engine" sandbox
(docker:dind), through `internal/dockerproxy` listening on `~/.gvx/docker.sock`
(or `127.0.0.1:<port>`). In the experimental mode **the app itself answers the
Docker Engine API**. Each container is its own gvx sandbox, run by a supervisor
of the engine's own over `rungv run`. Images are rungv's image cache and volumes are
rungv volumes. No dockerd runs anywhere. The mode is off by default. With it off, the app
behaves exactly as today.

With `rungv` v0.20+ (control protocol, see `docs/design/gvx-v020.md`), the engine
also supports `exec`, `cp` (archive), `pause`/`unpause`, `top`, `update`, guest
`kill`, graceful `stop` (honoring `StopSignal` and timeout), guest-accurate
`stats`, and live `network connect`/`disconnect` for containers on the overlay.
With `rungv` v0.19, those endpoints answer 501 as before. Still not supported:
`commit`, `export`, `diff` (`/changes`), WebSocket attach, swarm and plugins
(all answer 501).
Image builds are supported through the engine's lazy BuildKit builder sandbox
(§9.1). Containers on a user network reach each other by name through the
overlay (§9.2).

## 2. rungv facts this design relies on (verified with rungv v0.18 on this machine)

Checked with `~/.gvx/bin/rungv <cmd> -h` and real runs in a temporary `-cache-dir`:

- `run [flags] IMAGE [args...]` (the image may be positional). `-entrypoint X` takes
  **one executable** (`-entrypoint "/bin/echo x"` fails: *executable "/bin/echo x"
  not found*). With `-entrypoint`, the image's Cmd is **cleared**:
  `-entrypoint /bin/echo alpine` prints an empty line. So the engine computes the
  final argv itself and passes `-entrypoint argv[0] IMAGE argv[1:]...`. That is
  `Spec{SetEntrypoint: true, Entrypoint: argv[0], Command: rungv.FormatArgs(argv[1:])}`.
- The exit code of the guest is rungv's exit code (`sh -c 'exit 3'` → 3).
- Standard input passes through (`echo hello | rungv run alpine cat` → hello).
- rungv writes its own lines to **stderr**, mixed with the guest's stderr:
  `rungv: platform=hvf`, `rungv: publishing ADDR -> PORT/proto`,
  `gvx: warning: could not auto-publish ...`. Fatal errors look like
  `rungv run: gvx: executable ... not found`. Those error lines are kept for the user.
- `-t` and `-i` are separate flags (there is also a combined `-it`). With
  `-t -i`, stdin and stdout on a host PTY slave, and stderr on a pipe:
  - the guest's terminal output (prompt `/ # `, echo, stdout) comes on the PTY;
  - **the guest's stderr comes on rungv's stderr** (with `\r\n`), not on the PTY;
  - `stty size` in the guest reports the host PTY size;
  - **resize reaches the guest only when the PTY is rungv's controlling
    terminal**: `SysProcAttr{Setsid: true, Setctty: true}` with the slave on
    fd 0. Without it the guest kept the old size.
- `-d` is refused. There is no exec, no build, and no network between sandboxes (the engine
  adds one, §9.2). Each run has its own NAT: guest 10.0.2.15, host loopback 10.0.2.2.
- `images --no-trunc --format json` gives full `sha256:` IDs, but `Size` is a
  human string ("9.155MB"). `inspect REF...` gives a JSON array with `Id`, `RepoTags`,
  `RepoDigests`, `Created`, `Architecture`, `Os`,
  `Config{Env,Cmd,Entrypoint,WorkingDir,User,ExposedPorts?}`, `RootFS`, and `Size` (bytes).
  Empty config maps are omitted.
- `volume create|ls|inspect|compact|rm|prune` take `-cache-dir`, and the volumes live
  in `<cache-dir>/volumes/<name>.ext4`. `volume ls --format json` has `SizeBytes`,
  `AllocatedBytes`, `InUse`, `LockMode`. **`volume prune` deletes every unlocked
  volume, including those of the user's own sandboxes. The engine must never call
  it.** The same goes for `image prune -a` and `system prune`.
- `tag SRC DST`, `save [-o FILE] IMAGE...` (only images that came from a
  registry), `load [-i FILE] [-q]` (reads stdin), `pull [-platform]`, `rmi [-f]`,
  `image prune [-a] -f`, and `system df --format json` all exist.
- creack/pty v1.1.24 has no Windows support: on Windows a TTY is a pseudoconsole (ConPTY) of the
  supervisor's own, `pty_windows.go`.

## 3. Package layout

```
internal/engine/            the in-app Docker Engine API (new)
  engine.go                 Engine, Config, New/Open/Close/Handler, notify/changes   [frozen]
  runner.go                 Runner interface, rungvRunner, NewRunner                 [frozen]
  router.go                 routing table, version prefix, 501 table                 [frozen]
  httpx.go                  errors, JSON, query and filter helpers                   [frozen]
  store.go                  atomic JSON files under Config.Dir                       [frozen]
  shared.go                 helpers several workstreams need (randomHex64, ...)      [frozen]
  fake_test.go              the fake Runner + testEngine helpers                     [frozen]
  harness_test.go           real-CLI harness (GVX_ENGINE_HARNESS=1)                  [frozen]
  router_test.go            routing tests                                            [frozen]
  runner_proc.go            rungvRunner process methods → Supervisor                 A
  streams.go, attach.go     output capture, logs, attach, wait, resize, stats        A
  container_types.go        Container record and docker config types                 B
  containers.go             container state, lifecycle, handlers                     B
  runner_store.go           rungvRunner image/volume methods → rungv                 C
  images.go volumes.go networks.go events.go system.go                               C
internal/sandbox/
  spec.go                   + Interactive, TTY, ContainerID; RunArgs adds -i/-t       (done; A owns after)
  supervisor.go             + Supervisor.Output, State.Signal (fields only)          (done; A owns after)
  stdio.go                  Output; Stdin/CloseStdin/Resize/Signal stubs             A
internal/dockerproxy/proxy.go  SetLocal/Local/NotifyChanged                          (done; D owns after)
internal/store/store.go        Settings.SandboxEngine                                (done; D owns after)
internal/ui/...                wiring, gating, Settings UI, i18n                     D
```

The engine has a **supervisor of its own** (built by `NewRunner`), separate from the
app's `m.sup`. So container sandboxes never appear among the user's sandboxes,
never receive the app's `Prepare` hook or announce toasts, and stop with the
engine. Every container spec also carries `Spec.ContainerID` as a marker.

## 4. Core types

### 4.1 Container record (`container_types.go`, B)

`Container{ID, Name, Created, Image, ImageID, Config, HostConfig, Networks,
Mounts, Spec, Ports, State, RestartCount}`. See the file for the fields. The
fields marked "relied on" are read by A and C and stay as they are.

- **ID**: 64 hex digits from crypto/rand. It is also `Spec.ID` and the runner key.
  Lookup takes the full ID, the name with or without `/`, or a unique ID prefix. An ambiguous
  prefix is a 404 that says so.
- **Name**: `^[a-zA-Z0-9][a-zA-Z0-9_.-]+$` (the same as `sandbox.nameRe`), unique. A
  conflict is a 409 `Conflict. The container name "/x" is already in use by container "<id>".
  You have to remove (or rename) that container to be able to reuse that name.`
  Without `?name=`, the engine generates a docker-style `adjective_surname` name. B writes its
  **own** word lists; do not copy Docker's names-generator.
- **Config** is the merged config (§6.1): what runs, and what inspect shows.
- **Spec** is the sandbox spec built at create. Ports are filled at each start.
  Its fields: `ID`=container ID, `Name`=container name,
  `Image`=ImageID's reference (the ref the client gave), `Pull: "never"`
  (create made sure the image is cached), `Hostname`=Config.Hostname or ID[:12],
  `User`, `Workdir`, `Env` (merged; every entry `KEY=VALUE`, see §6.1),
  `SetEntrypoint/Entrypoint/Command` from the argv, `Mounts`, `Memory`
  (bytes → `"<n>b"`), `CPUs` (NanoCpus/1e9), `Network` (`none` for NetworkMode
  none, otherwise nat), `Unprivileged: !Privileged`, `Interactive: OpenStdin`,
  `TTY: Tty`, `ContainerID`, and `-read-only` through `ExtraArgs` when
  ReadonlyRootfs is set.

### 4.2 State machine (B)

```
            create                start ok                 run ends
  (none) ──────────► created ───────────────► running ──────────────────► exited
                        ▲  │                    │  ▲                       │  │
                        │  │ rm                 │  │ restart policy says   │  │ start
                        │  ▼                    │  │ go (after backoff)    │  │
                        │ (gone)          stop/kill  └──── restarting ◄────┘  │
                        │                       ▼                             │
                        │                     exited ◄──────────────────────── ┘
  rm (-f stops first): any → removing → (gone)  emits destroy; AutoRemove does this on exit
```

- Status words are docker's (`created`, `running`, `restarting`, `removing`,
  `exited`, `dead`). `paused` never happens.
- Runner state → status: `Starting/Running/Stopping` → running;
  `Stopped/Exited/Failed` → exited.
- Exit code: `State.ExitCode`. When `ExitCode == -1 && State.Signal != 0`, the exit code is
  `128+Signal` (kill → 137). A sets `State.Signal` in the supervisor.
- `State.Exits++` at every end of a run. `wait?condition=next-exit` waits
  for it to grow. `StoppedByUser` is set by stop/kill and cleared by start.
- `start` of a running container → 304. `stop` of a stopped one → 304.
- `Error` is set only when `runner.Start` itself failed.

### 4.3 Persisted store (`Config.Dir`, app: `<app config dir>/gvx-desktop/engine`)

| file | owner | format |
|---|---|---|
| `containers.json` | B | `{"version":1,"containers":[Container...]}`, oldest first, rewritten (saveFile, atomic) on every change of a record |
| `volumes.json` | C | `{"version":1,"volumes":[Volume...]}`: the volumes the engine made, with labels and options |
| `networks.json` | C | `{"version":1,"networks":[Network...]}`: user networks and their members; builtins are not stored |
| `logs/<id>.log` | A | docker json-file lines `{"log":"...","stream":"stdout","time":"RFC3339Nano"}`, capped (≈16 MiB, then drop the oldest half) |
| `run/`, `snapshots/` | supervisor | unused (no checkpoints) |

At Open, a record whose status was running/restarting/removing becomes
`exited` (ExitCode 255 when it was not stopped cleanly, FinishedAt = now) and is saved.
After that, restart policies apply (§7).

## 5. The Runner (`runner.go`, frozen)

The engine talks to the world only through `Runner`. It covers process control (the
supervisor) and rungv's image cache and volumes. Read the interface in the file.
The two halves of the real implementation `rungvRunner` live in `runner_proc.go` (A)
and `runner_store.go` (C).

- `SetHandlers(event, output)` is called once by `New`. The real runner sets
  `Supervisor.Notify` and `Supervisor.Output`.
- `Images` must return full IDs (`--no-trunc`). `InspectImages` decodes rungv
  inspect into `ImageInspect` (images.go). A missing image returns an error, which
  `imageConfig` turns into a 404 `No such image: <ref>`.
- Image and volume commands go through `rungv.Client` (`client().Command`/`Stream`).
  C may add helpers in a new file `internal/rungv/engine.go` (C-owned) and must not edit
  `rungv.go`.

The fake (`fake_test.go`) runs no process. A `fakeProc` is driven by
`fakeRunner.Script` (write output, read stdin, exit with a code). Stop ends a run
with 130 as Stopped. Signal SIGKILL/TERM/INT ends it with `-sig` (ExitCode -1,
Signal set). Images, volumes and pulls are kept in maps. `newTestEngine(t)` gives an open
engine plus an httptest server and `te.do(t, method, path, body)`.
`reopenTestEngine(t, dir, fake)` tests persistence. Add helpers in your own test
files; methods on `*fakeRunner`/`*fakeProc` may be declared there too.

## 6. Containers (B)

### 6.1 Image config merge (create)

`imageConfig(ctx, Config.Image)` (C) gives the image's `ImageConfig`. Docker's rules
(daemon `merge` + create):

1. **Entrypoint/Cmd**: if the request's `Entrypoint` has no elements (nil or
   `[]`), then: if its `Cmd` is empty, take the image's Cmd; if its
   `Entrypoint` is **nil** (absent or null), take the image's Entrypoint. An
   explicit `[]` keeps it empty. A non-empty request Entrypoint keeps the request's Cmd as
   is, even an empty one. `Entrypoint == [""]` (the CLI's `--entrypoint ""`) means
   no entrypoint. **argv = Entrypoint + Cmd**. An empty argv is a 400
   `No command specified`.
2. **Env**: the image's Env, then the request's entries replace same-key entries and
   append new ones. Entries **without `=` are dropped**: rungv would copy them
   from the host's environment, which leaks the app's environment into the
   container. If no `PATH` remains, add
   `PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`.
3. **WorkingDir**: the request's, else the image's, else `/`. **User**: the request's, else the image's.
4. **Labels**: the image's labels, overlaid by the request's. **ExposedPorts** and **Volumes**: the union of both.
5. **Hostname**: the request's, else ID[:12]. StopSignal and Healthcheck are kept for
   inspect and otherwise ignored (Healthcheck → a Warning).
6. `Config.Image` stays the client's reference. `ImageID` is the inspect `Id`.

### 6.2 Ports

- For each `HostConfig.PortBindings["80/tcp"]` entry, the host IP is `""`, `0.0.0.0`,
  `::` or `127.0.0.1` → 127.0.0.1, silently; containers are reachable from this
  computer only. Any other IP → 127.0.0.1 plus the Warning `Port 80/tcp is published on
  127.0.0.1 only, not on <ip>`. An empty HostPort → a free port, found by listening on
  `127.0.0.1:0` and closing, chosen **at each start** (as docker does). A host
  port range in one binding is expanded.
- `PublishAllPorts`: every ExposedPort without a binding gets a free port.
- Before `runner.Start`, B checks each fixed host port by listening on it. A busy one
  makes start fail with 500 `Bind for 127.0.0.1:<p> failed: port is already
  allocated`. Specs go as `-p 127.0.0.1:<host>:<guest>/<proto>`.
- `Container.Ports` (inspect `NetworkSettings.Ports`, list `Ports`) come from what was
  passed. They may be confirmed against `State.Ports` (rungv's "publishing" lines).
- `NetworkMode: host` is treated like bridge (NAT) with a Warning. `none` → `-net none`, and
  `none` with published ports is a 400.

### 6.3 Mounts

- `Binds` `src:dst[:opts]`: an absolute host path (on Windows, also `C:\...` and
  `C:/...`; split after the drive colon) → bind mount (`sandbox.MountBind`). Any
  other src → a named volume (`ensureVolume`, C). Opts `ro`/`rw` are honored; `z`,
  `Z`, `cached`, `delegated`, `consistent`, propagation and `nocopy` are ignored.
  **No shared-folder check and no Windows path rewrite**: those are dind
  concerns, and rungv mounts host paths directly. rungv creates missing bind
  directories.
- `Mounts`: `bind` (a Source that does not exist is a 400 `invalid mount config for
  type "bind": bind source path does not exist: <src>`, as docker does), `volume`
  (an empty Source → an anonymous volume), and `tmpfs` (`TmpfsOptions.SizeBytes` →
  `size=<n>`). Other types → 400.
- `Tmpfs` map `path → "size=64m,ro"` → tmpfs mounts.
- Each image `Volumes` path not covered by a mount → an anonymous volume named by 64
  hex digits (`ensureVolume(..., anonymous=true)`), recorded in
  `MountPoint.Anonymous`. These volumes are deleted by `rm -v` and by AutoRemove.
- Two mounts on one target → 400 (`Spec.Validate` says so too).
- A volume can be mounted by one running sandbox at a time. rungv enforces this (and so does
  the supervisor, within one supervisor). Start then fails with rungv's message,
  which B passes on as a 409.

### 6.4 Networks at create (B → C)

The default NetworkMode (`""`, `default`, `bridge`) → the network `bridge`. A user network
(NetworkMode, or the one entry of `NetworkingConfig.EndpointsConfig`) must exist.
`network(ref)` returns a 404 for a missing one. `connectNetwork` records the membership with its
aliases and, on a user network, gives the container an address of the network's subnet
(`EndpointsConfig[n].IPAMConfig.IPv4Address` when the client asks for one). Inspect shows
`NetworkSettings.Networks[name] = {NetworkID, Gateway, IPAddress, IPPrefixLen, Aliases,
DNSNames}`: the subnet's first address and the container's on a user network, `10.0.2.2` and
`10.0.2.15/24` (the sandbox's NAT) on the builtin bridge. Removing a container calls
`disconnectNetworks`. See §9.2.

### 6.5 Lifecycle endpoints

- `start`: compute the ports, check them, `runner.Start(spec)`, status running, `StartedAt`,
  emit `start`. A failed Start → 500 with the cause, and the record is exited with Error.
- `stop?t=`: StoppedByUser, then `runner.Stop(id, t)` (t default 10 s, or
  Config.StopTimeout). It returns once the container has ended. `restart?t=` = stop
  + start. `kill?signal=` (default SIGKILL; names with or without `SIG`, or
  numbers) → `runner.Signal`. Kill of a container that is not running → 409.
- `rm?force=&v=`: a running container → 409 unless force (then stop with t=0, i.e.
  kill). Status removing, `runner.Forget`, `dropLogs` (A), anonymous volumes when `v`
  (`removeVolume`, C), `disconnectNetworks` (C), delete the record, save, emit
  `destroy`, `notify()`.
- BuildKit guard (§14.8): inspect of a missing `buildx_buildkit_*` container,
  and create of such a name or of a `moby/buildkit` image, answer 501 "build".
- `rename?name=`: same name rules. The sandbox keeps its old rungv `--name` until
  the next start.
- `prune` (filters `until`, `label`) → `{ContainersDeleted, SpaceReclaimed: 0}`.
- After every change: save, emit, and `e.notify()` (wakes waiters and `Config.Changed`).

### 6.6 List and inspect

`GET /containers/json?all=&limit=&size=&filters=`. Filters: `id` (prefix), `name`
(substring match on the name without `/`; docker's is a regex: substring is enough),
`label`/`label!` (`filters.matchLabels`), `status`, `ancestor`
(image ref or ID), `exited`, `network`, `volume`. Without `all`, only running
containers. Newest first. Each row has `Id`, `Names: ["/name"]`, `Image`, `ImageID`,
`Command` (argv joined), `Created` (unix), `Ports: [{IP, PrivatePort, PublicPort,
Type}]`, `Labels`, `State`, `Status` ("Up 3 minutes", "Exited (0) 2 seconds
ago", "Created"), `HostConfig{NetworkMode}`, `NetworkSettings{Networks}`, and `Mounts`.

`GET /containers/{id}/json` returns `Id`, `Created` (RFC 3339), `Path`, `Args`, `State{Status, Running,
Paused, Restarting, OOMKilled, Dead, Pid, ExitCode, Error, StartedAt,
FinishedAt}` (zero times as `0001-01-01T00:00:00Z`), `Image` (ImageID), `Name`
("/name"), `RestartCount`, `Driver: "gvx"`, `Platform: "linux"`, `Config`,
`HostConfig`, `NetworkSettings{Ports, Networks, IPAddress, Gateway}`, `Mounts`,
`LogPath`, and `HostnamePath/HostsPath/ResolvConfPath: ""`. The `Config.Tty` flag must be right:
the CLI and compose pick the log framing by it.

## 7. Restart policies, AutoRemove, Open/Close (B)

- On a runner `EventExited` for a container (`containerEvent`): update the State
  (ExitCode, FinishedAt, Exits++), save, and emit `die` with `exitCode`. Then:
  1. if the container is being removed or stopped by the user → nothing more;
  2. `AutoRemove` → remove it (with its anonymous volumes) and emit `destroy`;
  3. restart policy: `always` and `unless-stopped` → restart; `on-failure[:N]` →
     restart when ExitCode != 0 and (N == 0 or RestartCount < N); `no`/"" →
     nothing. Restarts run after a backoff of 100 ms that doubles up to 1 min. The backoff
     resets when a run lasted ≥ 10 s. Status `restarting` during the backoff,
     and `RestartCount++` at each restart.
- `resumeContainers` (after Open): `always` → start; `unless-stopped` → start
  unless StoppedByUser. `on-failure` and `no` stay exited.
- `stopContainers` (Close, when the switch goes off or the app quits): stop all in
  parallel with the timeout. StoppedByUser is **not** set, so a later Open
  restarts the `always`/`unless-stopped` ones.
- AutoRemove and restart policies are mutually exclusive: create returns a 400
  `can't create 'AutoRemove' container with restart policy`, as docker does.

## 8. Streams: supervisor stdio, logs, attach, wait, resize, stats (A)

### 8.1 Supervisor changes (`internal/sandbox`)

Existing sandboxes (`Interactive == TTY == false`) must behave **exactly** as today:
an empty stdin pipe that stays open, line logs in LogBuffer, the same
States, events and port parsing. The `supervisor_test.go` tests keep passing unchanged.

- `Spec.Interactive` (`-i`): keep the write end of the stdin pipe for
  `Stdin(id)`. `CloseStdin` closes it (EOF in the guest).
- `Spec.TTY` (`-t`): `pty.Open()`. The slave goes on rungv's stdin **and** stdout, with
  `SysProcAttr{Setsid, Setctty}` (unix-only file `pty_unix.go`) so resize reaches the
  guest. stderr stays a pipe. The master is read as Stdout output. `Stdin` writes to the
  master. `CloseStdin` writes `0x04` (EOT). `Resize` → `pty.Setsize`. Close the slave in
  the parent after Start. Treat EIO from the master as EOF. On Windows
  (`pty_windows.go`), rungv runs on a pseudoconsole instead: stdin and stdout are the
  console's, stderr stays the caller's pipe, `Resize` is `ResizePseudoConsole`, and the
  console is closed when rungv exits, which ends its output. exec.Cmd cannot start a
  process on a pseudoconsole, so `startCmd`/`waitCmd` do it (CreateProcess with
  `PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE` and a handle list of stderr alone).
  Initial size: B calls `runner.Resize` right after Start when
  `HostConfig.ConsoleSize` is set. The CLI also resizes after start.
- Output: `pump` reads **raw chunks**, not lines. Each chunk goes (a) to
  `Supervisor.Output` as it comes, and (b) through a line splitter with carry into LogBuffer
  (same lines as the bufio.Scanner gives today: trailing `\r` trimmed, the 1 MiB
  line cap). On stderr, rungv's own lines (`rungv: ` and `gvx: ` prefixes) are not given to
  `Output`. Use a prefix filter that holds back only the bytes that could still be
  such a prefix, so guest stderr is not delayed by a line. They still go to
  LogBuffer and to the port/platform parsing.
- `Signal(id, sig)`: `proc.Signal(sig)`; it does not set `stopping`.
- `State.Signal`: in `exited`, from `ProcessState.Sys().(syscall.WaitStatus)`
  when it was signaled.
- `Supervisor.Stop` keeps sending SIGINT (rungv ends the guest on it).

### 8.2 Engine side

- `onRunnerOutput(id, o)`: append to the container's log (memory ring + `logs/<id>.log`,
  with timestamps) and fan out to the attached clients. A slow client gets dropped
  output, not a blocked pump: each subscriber has a buffered channel, and the
  subscriber is closed on overflow.
- **Tty containers** have one stream: Stdout (terminal) and Stderr (the guest's
  stderr lines) are merged as raw bytes. Non-Tty containers keep the two
  streams apart.
- **Logs** `GET /containers/{id}/logs?follow&stdout&stderr&timestamps&tail&since&until`:
  a 200 with `Content-Type: application/vnd.docker.multiplexed-stream` (non-Tty, 8-byte
  frames `[stream,0,0,0,len BE32]`, 1 = stdout, 2 = stderr) or
  `application/vnd.docker.raw-stream` (Tty). `timestamps` prefixes each line with
  RFC3339Nano and a space. `tail=N|all` counts lines. `follow` streams until the container stops
  (after its last run ended, the stream ends) or the client goes away. Neither
  stdout nor stderr → 400 `You must choose at least one stream`.
- **Attach** `POST /containers/{id}/attach?stream&stdin&stdout&stderr&logs&detachKeys`:
  hijack through `http.NewResponseController(w).Hijack()`. Through the proxy, the
  writer is wrapped, and ResponseController unwraps it. When the request has
  `Upgrade: tcp`, answer `HTTP/1.1 101 UPGRADED\r\nContent-Type: <ct>\r\nConnection:
  Upgrade\r\nUpgrade: tcp\r\n\r\n`, else `HTTP/1.1 200 OK` with the same content
  type. `<ct>` is raw-stream for Tty, else multiplexed-stream when
  `requestVersion(r) >= 1.42`, else raw-stream with frames. `logs=1` replays the log
  first. stdin: copy the client's bytes to `runner.Stdin(id)`. An attach may come **before
  start** (`docker run`): buffer until the run exists by waiting on
  `e.changes()`. When the client half-closes (EOF on read) and `Config.StdinOnce`,
  call `runner.CloseStdin`. Keep the output flowing until the run ends, then
  CloseWrite and close, with the same half-close behavior as
  `dockerproxy.serveHijack`. detachKeys (default ctrl-p ctrl-q) on a Tty
  detach the client without stopping the container.
- **Wait** `POST /containers/{id}/wait?condition=not-running|next-exit|removed`:
  write the 200 header and **flush at once** (the CLI calls wait before start), then block
  on `e.changes()` until the condition holds, then write
  `{"StatusCode":N,"Error":null}`. `not-running` (the default): return now if the container is not
  running. `next-exit`: wait for `State.Exits` to grow. `removed`: wait for
  `lookup` to return a 404 (for an AutoRemove container, after its exit). A missing
  container at the call → 404.
- **Resize** `POST /containers/{id}/resize?h=&w=` → `runner.Resize`. A container that is not
  running → 409. One without a TTY → 200 without doing anything (docker answers 500, the CLI
  ignores it).
- **Stats** `GET /containers/{id}/stats?stream=&one-shot=`: one JSON object, or one per
  second when streaming, shaped as `dockerapi.Stats` parses it (`read`, `cpu_stats`,
  `precpu_stats`, `memory_stats{usage,limit}`, `networks`, `pids_stats`). Values come from
  `internal/procstat` on the rungv PID when that is cheap, zeros otherwise.

## 9. Images, volumes, networks, system, events (C)

- `/images/json?all&filters(reference,dangling,label,before,since)`: one row per
  image ID, with `RepoTags`, `RepoDigests`, `Created` (unix), `Size` (bytes, from
  InspectImages over all IDs in one call), `Containers` (-1, or a count from
  allContainers), `Labels`, `ParentId: ""`, `SharedSize: -1`.
- `POST /images/create?fromImage=&tag=&platform=`: stream `application/json` lines
  translated from rungv pull's lines: `{"status":"<line>"}`, plus
  `{"status":"<id>: Pulling from ...","id":...}` when one can be parsed, and a final
  `{"status":"Status: Downloaded newer image for <ref>"}`. Errors go in the stream as
  `{"errorDetail":{"message":m},"error":m}`. A pull that fails before streaming →
  404 for a missing repository. `fromSrc` (import) → 501. `X-Registry-Auth` is
  ignored; rungv uses its own credentials (rungv login). Emit image `pull`.
- `GET /images/{name}/json` → the inspect JSON plus the docker fields the CLI reads
  (`Id`, `RepoTags`, `RepoDigests`, `Created`, `Architecture`, `Os`, `Config`,
  `RootFS`, `Size`, `Metadata: {}`). A missing image → 404 `No such image: <name>`.
- `DELETE /images/{name}?force&noprune` → `[{"Untagged":ref},{"Deleted":id}]`. An image
  used by a container record → 409 `conflict: unable to remove repository reference
  "<ref>" (must force) - container <id12> is using its referenced image <id12>`,
  unless force. Emit `untag`/`delete`.
- `POST /images/{name}/tag?repo=&tag=` → 201. Emit `tag`.
- `POST /images/load?quiet` → stream the body to `rungv load` and answer
  `{"stream":"Loaded image: ..."}` lines. `GET /images/get?names=` and
  `/images/{name}/get` → `application/x-tar` from `rungv save` (images without a
  registry source fail; pass on rungv's message).
- `POST /images/prune?filters(dangling)`: `dangling=true` (the default) → `PruneImages`
  (rungv `image prune -f`, dangling only). `dangling=false` (`-a`) → remove each
  image that no container record uses and `Config.KeepImages()` does not name, one
  by one with `RemoveImage`. **Never `rungv image prune -a`.** Answer
  `{ImagesDeleted, SpaceReclaimed}`.
- **Volumes** (`volumes.json`): the engine lists **only the volumes it made**. rungv
  volumes of the user's sandboxes share the cache directory and stay out of reach.
  A container may still mount one by name; `ensureVolume` then uses it without
  recording it. `GET /volumes?filters(name,label,dangling,driver)` →
  `{Volumes:[{Name, Driver:"local", Mountpoint, CreatedAt, Labels, Scope:"local",
  Options}], Warnings:[]}`. `POST /volumes/create{Name, Driver, DriverOpts,
  Labels}`: a Driver other than "local" or "" is a 400 (the DriverOpt `type=hostfs` picks rungv's
  hostfs driver). An empty Name → 64 hex. An existing name answers the existing volume (docker
  does so). Emit `create`. `DELETE /volumes/{name}?force`: a volume mounted by a container
  record → 409 `remove <name>: volume is in use - [<ids>]`. A volume the engine did not make
  → 404. Emit `destroy`. `POST /volumes/prune?filters(label, all)` removes engine
  volumes unused by any container: anonymous ones only, unless `all=true` (docker
  ≥ 23 behavior). Answer `{VolumesDeleted, SpaceReclaimed}`. **Never `rungv volume prune`.**
- **Networks**: the builtins `bridge` (Driver bridge), `host` (Driver host) and `none`
  (Driver null) have fixed IDs (fakeHex-like constants), and the user networks come from
  `networks.json`. List and inspect shapes: `Name, Id, Created, Scope:"local", Driver,
  EnableIPv6:false, IPAM{Driver:"default", Config:[{Subnet, Gateway}]}, Internal,
  Attachable, Ingress:false, Containers{id:{Name, EndpointID, MacAddress, IPv4Address}},
  Options, Labels`. The builtin bridge shows the NAT (`10.0.2.0/24`, `10.0.2.15/24`); a
  user network shows its subnet and the containers' addresses (§9.2). List filters:
  `name`, `id`, `label`, `driver`, `type` (custom|builtin), `dangling`. `create`
  → 201 `{Id, Warning:""}`, with `IPAM.Config[].Subnet/Gateway` honored (one IPv4
  subnet; an overlapping one → 403). A duplicate name → 409. `connect` gives the
  container an address and updates its record; `disconnect` takes them away. Both
  reach a running container's names and reachability at once. `delete` of a builtin → 403. A network with containers → 403
  `error while removing network: network <name> id <id> has active endpoints`.
  `prune` removes the unused user networks.
- **System**:
  - `GET|HEAD /_ping` → 200 `OK` (no body for HEAD). Headers: `Api-Version:
    1.52`, `Ostype: linux`, `Docker-Experimental: false`, `Builder-Version: 1`,
    `Swarm: inactive`, `Cache-Control: no-cache, no-store, must-revalidate`, `Pragma:
    no-cache`, `Content-Type: text/plain; charset=utf-8`. Only /_ping needs them.
  - `GET /version` → `{Platform:{Name:"GVX Desktop sandbox engine"},
    Components:[{Name:"Engine", Version, Details:{ApiVersion, MinAPIVersion, Os,
    Arch, GitCommit:""}}, {Name:"rungv", Version: <rungv version>}], Version:
    "gvx-desktop-<ver>", ApiVersion:"1.52", MinAPIVersion:"1.24", Os:"linux",
    Arch:<GOARCH>, KernelVersion:"gvx", GoVersion, BuildTime:"", Experimental:true}`.
  - `GET /info` → `ID` (stable, from Dir), `Name` (hostname),
    `ServerVersion:"gvx-desktop-<ver>"`, `Containers`, `ContainersRunning`,
    `ContainersPaused:0`, `ContainersStopped`, `Images`, `Driver:"gvx"`,
    `DockerRootDir:<Dir>`, `OperatingSystem:"GVX Desktop sandbox engine"`,
    `OSType:"linux"`, `Architecture`, `NCPU`, `MemTotal` (host), `KernelVersion`,
    `IndexServerAddress:"https://index.docker.io/v1/"`, `RegistryConfig:{}`,
    `Swarm:{LocalNodeState:"inactive"}`, `Plugins:{Volume:["local"],
    Network:["bridge","host","null"]}`, `SecurityOptions:[]`, `Runtimes:{}`,
    `DefaultRuntime:"gvx"`, `LoggingDriver:"json-file"`, `CgroupDriver:"none"`,
    `Warnings:[<one line naming what is experimental>]`.
  - `GET /system/df` → `{LayersSize, Images:[...rows], Containers:[...rows with
    SizeRw 0], Volumes:[{...UsageData{Size, RefCount}}], BuildCache:[]}`.
- **Events** (`events.go`): a ring of the last 1000 events plus subscribers.
  `GET /events?since&until&filters(type,event,container,image,volume,network,label)`
  streams `application/json` lines `{Type, Action, Actor:{ID, Attributes}, scope:
  "local", time, timeNano, status, id, from}` (the legacy fields too, for old
  clients). `since` replays from the ring. `until` in the past → replay then end.
  Everyone calls `emit`. It never blocks: a subscriber that is slow is closed.

### 9.1 Image builder (BuildKit sandbox, buildx integration, legacy `POST /build`)

`internal/engine/builder.go`, `builder_listen_{unix,windows}.go`, `build.go`, and
`internal/ui/model_buildx.go` add `docker build`, `docker buildx build`, and
`docker compose build` without running dockerd:

- **Engine-owned BuildKit sandbox**: the engine runs `moby/buildkit:v0.34.2`
  (`BuilderImage`, pinned to a runc 1.5-era release because older releases such
  as `v0.17.3` fail every `RUN` step under gVisor with
  `BPF_PROG_ATTACH not supported` on cgroup2). The sandbox uses an internal
  runner ID (`engine-buildkit`) that is not in `containers.json` and never
  appears in `docker ps` or user sandboxes. Its cache lives in the dedicated
  rungv volume `gvx-buildkit` mounted at `/var/lib/buildkit`, protected from
  every engine volume endpoint (`GET /volumes`, create/delete/prune, container
  mounts) and from the app's UI volume pruning via `Engine.VolumeOwnership()`.
- **Lazy front socket**: at `Engine.Open`, the engine binds a private front
  Unix socket (`~/.gvx/engine/buildkit.sock` in the app, `<dir>/buildkit.sock`
  in tests/harness) with mode `0600` using the same safe-listen rules as
  `dockerproxy.listenUnix` (never removing a live socket or a non-socket). On
  the first connection, the engine single-flight starts `buildkitd` publishing
  on `<socketDir>/buildkitd.sock` (`-p unix:<sock>:1234 --addr tcp://0.0.0.0:1234`),
  waits for `running server on` and socket readiness, and pipes bytes both ways
  with half-close propagation. After an idle period with zero connections
  (default 10 min, configurable via `Config.BuilderIdleTimeout`) or on
  `Engine.Close`, the builder sandbox stops and starts again lazily on the next
  build.
- **buildx integration (`gvx-desktop` builder)**: when sandbox-engine mode is
  on, the app quietly ensures a buildx remote builder named `gvx-desktop` exists
  with endpoint `unix://<front socket>` and `--driver-opt default-load=true` (so
  built images load into the engine via `POST /images/load`). If
  `aliases.builder` is not already `"buildx"` in `$DOCKER_CONFIG/config.json`
  (or `~/.docker/config.json`), it runs `docker buildx install` (so plain
  `docker build` honors per-context buildx defaults) and records
  `Settings.DockerBuilderAliasInstalled = true`. It also runs
  `docker -H <dockerHost> buildx use --default gvx-desktop` (so
  `docker compose build` and `DOCKER_HOST` sessions resolve `gvx-desktop`
  through `BUILDX_CONFIG/defaults/<sha256(endpoint)>` without `--global`), and,
  when the `gvx` docker context exists, records its previous builder in
  `Settings.DockerBuilderBefore` and runs
  `docker --context gvx buildx use --default gvx-desktop` (setting the
  per-context default under `defaults/<sha256("gvx")>` and `current` with
  `Global: false` so other contexts keep their own builders). This runs before
  `Engine.Open` binds `buildkit.sock` so `buildx inspect`/`create` probes do not
  eagerly start `buildkitd`. Turning sandbox-engine mode off closes the engine
  first and then clears the per-endpoint default (`-H <dockerHost> buildx use
  --default default`), restores the `gvx` context's previous builder (or `gvx`
  for dind's own builder), and runs `docker buildx uninstall` only if the app
  installed the alias and `aliases.builder` still points to `"buildx"` — never
  deleting user builders. Failures
  (missing `docker` or `buildx`) surface as a quiet status note in Settings →
  Docker → Experimental alongside the builder's live `idle`/`starting`/`running`
  state.
- **Legacy `POST /build`** (`DOCKER_BUILDKIT=0`, Docker SDKs, testcontainers):
  the engine extracts the tar/gzip/bzip2 context body (with path-traversal,
  symlink-escape, and size-cap checks) into a temp dir under `Config.Dir/builds`,
  opens an ephemeral loopback TCP bridge (`127.0.0.1:0` → `buildkitd.sock`) for
  the build's duration, and runs `buildctl` from `BuilderImage` as a one-shot
  internal sandbox reaching the bridge at `tcp://10.0.2.2:<port>` with
  `--frontend dockerfile.v0 --local context=/ctx --local dockerfile=/ctx` plus
  `dockerfile`, `buildargs`, `target`, `labels`, `nocache`, `pull`, `platform`,
  and `--output type=docker,name=<t>,dest=/out/image.tar`. Progress streams as
  Docker JSON lines (`{"stream":...}`), followed by loading `/out/image.tar`
  into rungv, tagging every `t=`, emitting `{"aux":{"ID":"sha256:..."}}` (or
  `{"errorDetail":...,"error":...}` on failure), and removing all temp dirs.

### 9.2 Networks between containers (`overlay.go`, `internal/overlay`)

Every sandbox has a NAT of its own (guest `10.0.2.15`, host loopback `10.0.2.2`), so
containers cannot reach each other through it. Containers on a **user network** do through
an overlay: a WireGuard link in each sandbox whose one peer is a hub in the app. The NAT stays
as it was, so the internet, published ports and `host.docker.internal` do not go through the
hub. Containers on the builtin bridge only (plain `docker run`) do not join it, as they have
no names on Docker either.

- **Addresses.** A user network gets a subnet at create: the client's
  (`IPAM.Config[0]`, IPv4 only), or the first free /24 of `10.89.0.0/16` (Podman's range).
  Its gateway is the first address, which nothing answers. A container gets the next free
  address at connect, or the one it asked for. `networks.json` keeps `subnet`, `gateway`
  and `ips` (container → address). Networks from before the overlay get a subnet when the
  engine opens, and containers' records follow (`reconcileEndpoints`).
- **Start.** A container with addresses joins the hub (`Fabric.Join`): a new key pair, a hub
  peer whose allowed IPs are the container's addresses, and a DNS server on
  `127.0.0.1:<port>`. The sandbox then runs **netinit**: the engine mounts it read-only at
  `/.gvx-netinit`, makes it the entrypoint with the container's argv after it, passes its
  config in `GVX_NET` (keys, hub endpoint `10.0.2.2:<port>`, addresses, the route
  `10.89.0.0/16` and any custom subnet, MTU, keepalive 25 s, the container's user), clears
  the spec's user, and adds `-host-dns 127.0.0.1:<port>` and `-tune net.nat.mtu=<MTU+80>`. The container's record and inspect
  are untouched.
- **netinit** (`internal/overlay/netinit`, static Linux binary, no dependencies) runs as root:
  it creates `gvx0` with rtnetlink (`IFLA_LINKINFO` without `NLA_F_NESTED`, which gVisor
  refuses), sets the key and the hub peer through the `wireguard` generic netlink family,
  adds the addresses, brings the link up, adds the routes, appends the extra hosts to
  `/etc/hosts`, takes the default route away from an isolated container, drops to the
  container's user
  (docker's rules: a name must exist, a bare uid gets gid 0, supplementary groups from
  `/etc/group`, HOME from passwd unless the container set it), removes `GVX_NET`, and
  executes the command, which becomes the sandbox's first process as before. A link that
  cannot be made is a warning on stderr; the command runs without the network. This works
  with `--privileged=false` (the default for containers).
- **Hub** (`hub.go`): wireguard-go over a UDP socket on `127.0.0.1` only (no firewall
  prompt) and a TUN of its own, the router. A packet a container sends comes out of the
  device; the router keeps it when its source and destination share a network and hands it
  back, and the device sends it to the destination's peer. WireGuard checks that a source
  address belongs to the peer that sent it.
- **DNS** (`dns.go`): rungv's guest relay at `10.0.2.3` sends every query to the container's
  server. It answers, with TTL 10, the names (container name, aliases, short ID) of the
  running containers that share a network with the asker, and an empty answer for their
  other types. Every other query goes to the host's servers (`/etc/resolv.conf`; the
  adapters' servers on Windows), or to Go's resolver for A/AAAA when there are none.
- **Extra hosts.** `--add-host` / `extra_hosts` (`NAME:IP`, `NAME=IP`, `host-gateway` →
  `10.0.2.2`) are checked at create (400 when malformed) and added by netinit at each
  start. A container with extra hosts and no user network runs netinit without a link and
  without `-host-dns`.
- **Internal networks.** A container whose networks are all `--internal` (the builtin bridge
  counts as public) is isolated: netinit replaces eth0's default route with routes to
  `10.0.2.2` (the hub) and `10.0.2.3` (the DNS relay), and its DNS server answers the
  containers' names and refuses every other name, as Docker's does since 26. Ports
  published from it still work, as the NAT forwards them in.
- **Restarts.** A container leaves the hub inside the exit's critical section, before its
  record stops running, so a start that follows (restart, a restart policy) cannot have its
  join undone by that exit.
- **Changes.** `syncOverlay` gives the Fabric the containers' networks and names after
  connect, disconnect, rename and remove. A container leaves the hub when its sandbox
  exits or fails to start, and the hub stops when the engine closes. `docker network
  connect` on a running container reaches it at once for names and reachability; its link
  keeps its addresses until the next start, so its name answers one of those.
- **MTU.** Every packet costs a trip through both sandboxes' NAT relays and a system call
  in the hub, whatever its size (the hub's CPU goes to the UDP calls; ChaCha20 is under
  1%). The links use MTU 60000 and eth0 60080. 200 MB between two containers (a Go
  sender and sink, not busybox, which is the bottleneck itself) went across at 47 MB/s
  with 1420, 160-180 MB/s with 8920, and about 250 MB/s with 60000, against about 200
  MB/s from a container to the host through the NAT alone. A rungv without `-tune`
  keeps eth0 at 1500 and the links at 1420. The hub's socket buffers are raised (macOS
  starts a UDP socket with 9216 bytes), and the router queues at most 256 packets.
- **Limits.** IPv4 only. An isolated container still reaches the host's loopback through
  `10.0.2.2`, which the hub needs.
  `host` and `container:` network modes are as before (NAT with a warning, refused).

## 10. App wiring and UI (D)

- **Setting**: `store.Settings.SandboxEngine` (added). Default off.
- **Model**: D adds `engine *engine.Engine`, built in `NewModel` with
  `engine.New(engine.Config{Dir: filepath.Join(dataDir, "engine"), Runner:
  engine.NewRunner(m.Client, filepath.Join(dataDir, "engine")), Version: <app version>,
  Changed: m.dockerProxy.NotifyChanged, KeepImages: <images of m.Specs>})`.
  `dataDir` is the app's store dir. Tests and render use a temp dir. `Offline` never opens it.
- **Switching**, at start when the setting is on and on toggle:
  - on: `m.engine.Open(ctx)` (async), then `m.dockerProxy.SetLocal(m.engine.Handler())`.
  - off: `m.dockerProxy.SetLocal(nil)`, then `m.engine.Close(stopTimeout)` (async).
    The engine's containers stop. Their records stay and come back when the mode is on again.
  - Quit: `m.engine.Close(stopTimeout)` with the other sandboxes.
  - The proxy keeps its listener and address. `SetLocal` bypasses `Check` and
    `Rewrite` (the shared-folder check and the Windows bind rewrite), which are
    dind concerns. `Changed` fires on the mutating requests the engine answers
    (the proxy records the status), and on the engine's own changes through
    `Config.Changed`.
- **The app's dockerapi client**: in sandbox-engine mode `dockerBackendHost()`
  returns `m.dockerHost()`, the proxy's front endpoint, which then serves the
  engine. No dockerapi change is needed. When the listener failed
  (`DockerProxyErr`), the pages show that error, as the CLI would hit it too.
- **Gating**: `ContainersAccess` becomes `Combine(Rows, Settings, ...)`. In
  sandbox-engine mode it is always `ContainersOpen` (the engine is the app). The
  Docker Engine recipe page (engine_pages.go) shows an info banner: containers run
  as sandboxes, and the Docker Engine sandbox is not used while the experimental mode is on (do
  not stop the user's engine sandbox automatically). The Images page's "Docker
  Engine" pull target pulls through the engine: same code, since the host changed.
- **Container Terminal tab** (exec) and Explorer (archive): in sandbox-engine mode
  show "Not supported in the experimental sandbox engine" in place of the
  terminal/explorer. Do not call exec.
- **Settings → Docker → Experimental**: a switch "Run containers as sandboxes,
  without a Docker Engine" plus an honest description. What works: run (-d, -i, -t,
  --rm), ps, logs, stop/start/rm, ports on 127.0.0.1, volumes, bind mounts,
  pull/images, networks between containers, and Compose with several services. What
  doesn't: exec and cp (both work with rungv v0.20). When a Docker Engine sandbox is running, a note says it
  keeps running but docker no longer reaches it.
- **i18n**: every new string through `tr`/`trn` with English keys, translated in all six
  catalogs `internal/ui/i18n_{ko,ja,zh_hans,zh_hant,de,hi}.go`. `TestCatalogsAreComplete`
  and `~/go/bin/ggui i18n check` must pass. API messages to the CLI stay English.
- **Render shots** (render.go): `settings-sandbox-engine` (switch on, Docker
  section in view) and `containers-sandbox-engine` (Containers page in the mode,
  with made-up containers and the Terminal tab's message). Run them with
  `./bin/gvx-desktop -render-dir <tmp> [-render-lang ko]`.
- The Sandboxes page needs no change: container sandboxes live in the engine's
  supervisor and store, not in `m.Specs`.

## 11. API endpoint table

Prefix: any `/v<major>.<minor>` is accepted and stripped; there is no version check.
Owner: A/B/C. "501" means `{"message":"<what> is not supported by GVX
Desktop's experimental sandbox engine yet"}`. Unknown paths answer 404 `page not found`.

| Method | Path | Owner | Notes (what the CLI / compose need) |
|---|---|---|---|
| GET, HEAD | /_ping | C | `OK`, `Api-Version: 1.52`; the CLI negotiates from this header |
| GET | /version | C | ApiVersion 1.52, MinAPIVersion 1.24 |
| GET | /info | C | compose reads it at start |
| GET | /events | C | JSON lines stream |
| GET | /system/df | C | |
| POST | /auth | – | 501 (use rungv login) |
| GET | /containers/json | B | `all`, `filters` (label!) — compose's main query |
| POST | /containers/create | B | `?name=&platform=`; body Config+HostConfig+NetworkingConfig; 201 `{Id, Warnings:[]}`; 404 `No such image: x` makes the CLI pull |
| GET | /containers/{id}/json | B | `Config.Tty`, `State`, `NetworkSettings.Ports` |
| POST | /containers/{id}/start | B | 204; 304 when running |
| POST | /containers/{id}/stop | B | `t`, `signal`; 204; 304 when stopped |
| POST | /containers/{id}/restart | B | `t`; 204 |
| POST | /containers/{id}/kill | B | `signal`; 204; 409 when not running |
| POST | /containers/{id}/rename | B | `name`; 204 |
| DELETE | /containers/{id} | B | `force`, `v`; 204 |
| POST | /containers/prune | B | |
| POST | /containers/{id}/attach | A | hijack: 101 UPGRADED (`Upgrade: tcp`) or 200; raw/multiplexed |
| POST | /containers/{id}/wait | A | `condition`; header flushed at once; `{"StatusCode":n,"Error":null}` |
| GET | /containers/{id}/logs | A | `follow,stdout,stderr,timestamps,tail,since,until`; multiplexed unless Tty |
| POST | /containers/{id}/resize | A | `h`, `w` |
| GET | /containers/{id}/stats | A | `stream`, `one-shot`; guest stats via `rungv stats` (v0.20+), procstat fallback |
| GET | /containers/{id}/top | W2 | `rungv top` (v0.20+; 501 on v0.19) |
| GET | /containers/{id}/changes, /export | – | 501 |
| POST | /containers/{id}/update, /pause, /unpause | W2 | `rungv update`/`pause`/`unpause` (v0.20+; 501 on v0.19) |
| HEAD,GET,PUT | /containers/{id}/archive | W2 | `rungv cp` for running containers (v0.20+; 501 on v0.19 or stopped) |
| POST | /containers/{id}/exec, /exec/{id}/start, /exec/{id}/resize; GET /exec/{id}/json | W2 | `rungv exec` (v0.20+; 501 "exec" on v0.19) |
| POST | /commit | – | 501 |
| GET | /images/json | C | |
| POST | /images/create | C | `fromImage`, `tag`, `platform`; JSON progress stream |
| GET | /images/{name}/json | C | name may hold slashes |
| POST | /images/{name}/tag | C | `repo`, `tag`; 201 |
| DELETE | /images/{name} | C | `force`, `noprune` |
| POST | /images/load | C | tar body → rungv load |
| GET | /images/get, /images/{name}/get | C | rungv save |
| POST | /images/prune | C | dangling only unless `dangling=false` (then the keep rules of §9) |
| GET | /images/search, /images/{name}/history; POST /images/{name}/push; GET /distribution/{name}/json | – | 501 |
| POST | /build | – | Legacy build over the engine's BuildKit builder (§9.1) |
| POST | /build/prune, /build/cancel, /session, /grpc | – | 501 pointing to `docker buildx use gvx-desktop` |
| GET | /volumes | C | `{Volumes, Warnings}` |
| POST | /volumes/create | C | 201 |
| GET, DELETE | /volumes/{name} | C | |
| PUT | /volumes/{name} | – | 501 |
| POST | /volumes/prune | C | |
| GET | /networks | C | builtins + user networks |
| POST | /networks/create | C | 201 `{Id, Warning}` |
| GET, DELETE | /networks/{id} | C | |
| POST | /networks/{id}/connect, /disconnect | C | address, names, reachability (§9.2) |
| POST | /networks/prune | C | |
| * | /swarm, /nodes, /services, /tasks, /secrets, /configs, /plugins (…) | – | 501 |

### Call sequences to support (verify against the real CLI with `Config.Logf`)

- `docker run --rm alpine echo ok`: (ping) → create (404 → images/create → create)
  → attach(stdout, stderr) → wait?condition=removed → start → [output] → exit
  → AutoRemove → wait returns → CLI exits with the code.
- `echo hi | docker run -i --rm alpine cat`: create (OpenStdin, StdinOnce,
  AttachStdin) → attach(stdin, stdout, stderr) → wait(removed) → start. The CLI
  half-closes after stdin → CloseStdin → cat ends.
- `docker run -it alpine sh`: create (Tty) → attach (101, raw) → wait(next-exit)
  → start → resize. The exit code comes from wait.
- `docker run -d --name x -p 18095:80 nginx:alpine`: create → start; prints the ID.
- `docker logs -f x`: inspect (Tty) → logs?follow=1. `docker ps -a`: containers/json?all=1.
- `docker compose up -d` (one service): ping, info, containers/json?filters=label project,
  networks?filters=name (create `<proj>_default` with compose labels), volumes
  inspect/create, images/{img}/json (pull when 404), create `<proj>-<svc>-1` with
  labels and `NetworkingConfig.EndpointsConfig{"<proj>_default":{Aliases}}`, start,
  and inspect. `compose ps` → containers/json + label filters. `compose logs` → logs per
  container. `compose down -v` → stop, delete?v=1&force, delete network, list volumes by
  label, and delete them. If compose calls something unexpected, the engine's trace shows it.
  Add the route through the coordinator.

## 12. Test plan

Unit tests run in `go test -short` and need no rungv: the fake runner, temp dirs.

- A: supervisor: an existing-behavior regression (`supervisor_test.go` unchanged and
  green); stdin pipe (Interactive: write, CloseStdin → EOF) and TTY (a pty, resize
  seen by the guest, EOT on CloseStdin) as **integration** tests with real rungv
  (skipped under -short or without rungv, with a temp `-cache-dir`); the line splitter
  and the rungv-prefix filter as units. Engine: logs framing (multiplexed and raw), tail,
  timestamps, follow ending at exit; attach before start with stdin and half-close
  (StdinOnce closes stdin; output keeps flowing until exit); attach to Tty (raw, 101 when
  upgraded, 200 otherwise); wait for all three conditions, including wait-before-start
  and removed after AutoRemove; resize reaching the fakeProc; stats shape parsed by
  `dockerapi`.
- B: create/start/inspect/list with label filters (both filter forms)/stop/kill
  (137)/wait (through A's handler once it lands, else State)/rm (+v anonymous volumes)/
  AutoRemove; name conflicts and generated names; ID prefixes; the merge table
  (Entrypoint nil vs [] vs [""], Cmd, Env override and dropping bare KEYs, WorkingDir,
  User, Labels); ports (empty HostPort → free, non-loopback → warning, busy → 500,
  PublishAllPorts); mounts (binds, named and anonymous volumes, tmpfs, bad type); restart
  policies with backoff (use a short backoff in tests); persistence via
  `reopenTestEngine` (a running container comes back exited; `always` restarts;
  `unless-stopped` stopped by the user stays stopped).
- C: ping headers and HEAD; version/info shapes; images list/inspect/pull stream/
  delete conflict/tag/load/prune keep rules; volumes CRUD, the in-use 409, prune leaving non-engine
  volumes; networks builtins, create/connect/delete rules, the create warning seam; events
  (since replay, filters, a stream receiving a container start); the 501 table
  (router_test has a start).
- D: proxy `SetLocal` (a request and a hijack reach the local handler; Check/Rewrite are
  not called; Changed fires; nil goes back to the backend) in `dockerproxy`;
  model gating in both modes; the Settings switch toggling `SetLocal`; i18n completeness; render shots.
- Real CLI: tests in each workstream's own files use `startHarness(t)` from
  `harness_test.go` (`GVX_ENGINE_HARNESS=1 go test -run Harness ./internal/engine`):
  a real runner, a temp cache `/tmp/gvxd-test-se/cache`, a per-run data dir,
  and the socket under `/tmp/gvxd-test-se-runs/`. `DOCKER_CONFIG` is a temp dir with the
  compose plugin linked. `h.docker(t, stdin, args...)`. `h.restart(t)` reopens the engine over the
  same data. The `-it` test opens a creack/pty pair and runs the docker CLI on its slave
  (`script` does not work in agent shells).

## 13. Workstreams

Every file has exactly one owner. **Frozen** files (marked at the top) are the
architect's: `engine.go`, `runner.go`, `router.go`, `httpx.go`, `store.go`, `shared.go`,
`fake_test.go`, `harness_test.go`, `router_test.go`. To change one, message the coordinator
(parent session) with the exact change. The coordinator applies it or assigns it. New files: a
workstream creates only files with its own prefixes (listed below), test files
included. **Seams** are the cross-workstream methods. Their signatures and doc comments
are frozen, and their bodies are the owner's. Until a seam is implemented, it panics or does
nothing, so write your tests against your own code first, and integrate as seams land.
Package-level helpers outside the frozen files carry the workstream's area in their name (`container…`, `image…`, `stream…`); a helper two workstreams need goes in `shared.go`. Rule against deadlocks: **never call another workstream's seam, `emit` or
`notify` while holding your own mutex.**

| WS | Scope | Owned files (existing + may create) | Seams it provides |
|---|---|---|---|
| **A** streams | Supervisor stdin/PTY/resize/signal/raw output; logs store; attach; wait; resize; stats | `internal/sandbox/supervisor.go`, `spec.go`, `stdio.go`, new `internal/sandbox/pty_*.go`, `stdio_*_test.go`, `supervisor_test.go`; `internal/engine/runner_proc.go`, `streams.go`, `attach.go`, new `stream_*.go`, `attach_*.go`, `logs_*.go` (+ `_test.go`) | `onRunnerOutput`, `streamsEvent`, `dropLogs`, `openStreams`, `closeStreams`; Supervisor `Stdin`/`CloseStdin`/`Resize`/`Signal`/`Output`, `State.Signal` |
| **B** containers | create, merge, ports, mounts, names/IDs, store, list+filters, inspect, start/stop/restart/kill/rm/rename/prune, restart policy, AutoRemove, Open/Close behavior, container events | `internal/engine/container_types.go`, `containers.go`, new `container_*.go` (+ `_test.go`) | `lookup`, `allContainers`, `containerEvent`, `loadContainers`, `resumeContainers`, `stopContainers` |
| **C** images, volumes, networks, system | images, volumes, networks (stub), ping/version/info/df, events bus and `/events`, rungv image/volume runner methods, **the final end-to-end verification** | `internal/engine/runner_store.go`, `images.go`, `volumes.go`, `networks.go`, `events.go`, `system.go`, new `image_*.go`, `volume_*.go`, `network_*.go`, `system_*.go`, `events_*.go` (+ `_test.go`); new `internal/rungv/engine.go` (+ test) | `imageConfig`, `ensureVolume`, `removeVolume`, `network`, `connectNetwork`, `disconnectNetworks`, `loadVolumes`, `loadNetworks`, `emit` |
| **D** app wiring and UI | Settings field use, proxy Local switch (+ tests), the Model's engine lifecycle, dockerapi host, gating, Docker Engine page banner, Terminal/Explorer messages, Settings UI, i18n in all six catalogs, render shots | `internal/dockerproxy/*` (all), `internal/store/store.go`, `internal/ui/*` (all), `render.go`, `main.go` | — |

Nobody else edits `internal/ui`, `internal/dockerproxy`, `render.go` or
`main.go`. If A, B or C need an app-side change, they ask D. If D needs an engine API
beyond `engine.New/Config/Open/Close/Handler/NewRunner/RunningCount/StopRunning/VolumeOwnership`, it asks the coordinator. (The last three read B and C internals from engine.go: `stopContainerByRef`, `Container.AnonymousVolumes`, `containerStoreFile`, `e.vol`, `volumesFile`; their owners tell the coordinator before changing them.)

### A — acceptance

- `go test -short ./internal/sandbox ./internal/engine` green. `supervisor_test.go`'s
  existing tests pass unchanged.
- Integration (real rungv, temp cache, skipped under -short): Interactive stdin
  round trip; TTY: a prompt seen without a newline, `stty size` follows Resize, exit code
  propagated; Signal(SIGKILL) → `State.Signal == 9`.
- Unit tests over the fake: logs (both framings, tail, timestamps, follow ends),
  attach-before-start with stdin and half-close, Tty attach raw over 101 and over 200, wait ×3,
  resize, stats.
- Real CLI, as soon as B and C have create/start/ping: `docker run --rm alpine:3.20
  echo ok`; `echo hi | docker run -i --rm alpine:3.20 cat`; `docker run -it`
  through a creack/pty (a tty shown, `exit 3` → 3); `docker logs` and `logs -f`.

### B — acceptance

- `go test -short ./internal/engine` green with the B tests of §12.
- Real CLI (with C's ping and images): `docker run -d --name gvxd-web -p 18095:80
  nginx:alpine` + `curl -s 127.0.0.1:18095`; `ps`, `ps -a`, `inspect`;
  `stop`/`start`/`rm -f`; a named volume written by one run and read by the next; a bind mount
  of a temp dir; `harness.restart` keeps a stopped container in `ps -a`, and an `always`
  one comes back running.

### C — acceptance

- `go test -short ./internal/engine` green with the C tests of §12.
- Real CLI: `docker version`, `docker info`, `pull alpine:3.20`, `images`,
  `rmi`, `tag`, `volume create/ls/inspect/rm`, `network ls/create/rm`, `system df`,
  `events` (sees a container start); `docker exec` and `docker build` print the 501
  message.
- **Final end-to-end verification (C runs it, once A, B and D report done)**:
  a harness test `system_e2e_test.go` (or a manual run against a held harness)
  covering the requirements' whole list. That is: version; info; pull; images; `run --rm`;
  `-i`; `-d` + curl on 18095; ps/ps -a/inspect; logs/-f; stop/start/rm -f; a volume
  across two runs; a bind mount; `-it` via a pty; exec and build 501; single-service
  `compose up -d` / `ps` / `logs` / `down -v` (a compose file in a temp dir, project
  `gvxd-compose`, port 18096); a harness restart keeping a stopped container. Then the
  full Must-pass list on the whole tree (gofmt, build, vet ×3, GOWORK=off build,
  `go test -short -tags ggui_debug ./...`, `ggui i18n check`). Then clean up
  `/tmp/gvxd-test-se*`, any `gvxd-*` processes, and the sockets.

### D — acceptance

- `dockerproxy` tests for SetLocal/NotifyChanged (request, hijack, Changed, nil
  fallback, Check/Rewrite bypassed). UI tests: ContainersAccess open in the mode
  whatever the rows; the switch calls Open+SetLocal and Close+SetLocal(nil) (inject a
  fake engine opener, or test the pure functions); the dockerBackendHost switch.
- `TestCatalogsAreComplete` green, `~/go/bin/ggui i18n check` with no errors, every
  new string in all six catalogs.
- Render shots `settings-sandbox-engine` and `containers-sandbox-engine` in en
  and ko look right (look at the PNGs).
- No launch of the app in normal mode. D's real checks are the render harness and unit tests.
  C's final e2e covers the engine through the CLI.

### Coordination protocol

- Owners report seam completion to the coordinator, who tells the others.
- A request for a change to a frozen file or another workstream's file goes to the coordinator: the
  file, the exact diff, and why.
- Everyone runs the Must-pass commands before reporting done. A red build caused
  by another workstream's file is reported, not fixed.

## 14. Decisions taken (the user may overturn them)

1. API version advertised: **1.52** (min 1.24). The CLI 29.10 (client 1.57) and compose 5.8
   negotiate down to it.
2. Wildcard host IPs (`0.0.0.0`, `::`, empty) publish on **127.0.0.1 only**, with no
   warning. Containers are not reachable from the LAN. Explicit other IPs get a warning.
3. The engine's volume listing shows **only volumes it made**. The user's sandbox
   volumes in the same rungv cache are mountable by name but not listed, removable or
   prunable. `image prune -a` keeps the images of the user's sandboxes
   (`Config.KeepImages`).
4. Turning the switch **off stops the engine's containers**, and the records stay. Turning it on
   again brings back the `always`/`unless-stopped` ones. The Docker Engine
   sandbox is never stopped or started automatically by the switch.
5. The app's own Docker pages reach the engine through the **proxy's front
   endpoint**, not an in-memory listener: one line in `dockerBackendHost()`.
6. Container logs are kept in `logs/<id>.log` (json-file format, capped), so `docker
   logs` survives an app restart.
7. On Windows, `-t` runs on a pseudoconsole (ConPTY) of the supervisor's own; creack/pty has
   none.
8. Builds run through the engine-owned lazy BuildKit sandbox (`moby/buildkit:v0.34.2`)
   exposed via the `gvx-desktop` buildx remote builder (`default-load=true`) for
   `docker build` / `docker buildx build` / `docker compose build`, and via a
   one-shot `buildctl` sandbox for legacy `POST /build` (`DOCKER_BUILDKIT=0`).
   Docker's built-in BuildKit driver endpoints (`/grpc`, `/session`) remain 501,
   and the guard against buildx's `docker-container` fallback (`buildx_buildkit_*`
   inspect/create and `moby/buildkit` container create) remains in place so an
   unconfigured client never pulls or boots a stray container; all of those 501
   answers now explain that builds go through the `gvx-desktop` builder and how
   to select it (`docker buildx use gvx-desktop`). `Builder-Version` stays `1`.
