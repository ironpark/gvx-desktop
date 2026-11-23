# Adopting rungv v0.20 (control protocol and runtime commands)

Status: design and compiling skeleton. Branch `feat/container-networks`.
Inputs: the orchestrator's `BRIEF.md`, `facts-rungv.md` (verified v0.20 behavior) and
`facts-app.md` (where the app can use it). The brief's SAFETY rules bind every workstream. In
short: never touch the user's running sandboxes (docker-engine, k3s, registry), never exec into,
signal, pause or stop them; never touch `~/.gvx`, `~/.docker`, `~/.kube`, the app's real data
dir, or the real rungv cache; never replace `~/.gvx/bin/rungv`; never run `rungv volume prune`.
Test sandboxes are named `gvxd-*`, ports only 18090-18099 and 15xxx, always a temp `-cache-dir`.
Never launch the app in normal mode (`-render-dir` only). No commits.

## 1. What changes

rungv v0.20 gives a running sandbox a **control socket** (gvx-ctrl/2) and Docker-style runtime
commands over it: `exec`, `cp`/`export`, `pause`/`unpause`, `ping`/`status`/`stats`/`top`,
`kill`/`stop`, `update`, `port`, `mount`, `events`. The app drives them as **subprocesses of the
rungv CLI**, as it drives rungv everywhere else (no gvx-ctrl/2 client of our own). With that:

- **Sandbox engine** (P1): `docker exec` (all forms), `docker cp`, `pause`/`unpause`, `top`,
  `update`, `kill` inside the guest, graceful `stop` honoring StopSignal and the timeout, real
  guest `stats`, and `docker network connect` reaching a running container's link at once.
- **User sandboxes** (P1): a Terminal tab, Pause/Resume, live memory/CPU/LAN-isolation changes
  from the editor, guest-accurate usage. The Containers page's Terminal tab works in
  sandbox-engine mode.
- P2 (designed in §8; built only if cheap): dynamic ports, a Files view, recipe changes applied
  live, `rungv events` in place of stderr parsing.

With the user's **v0.19** rungv everything behaves exactly as today: no new flag is passed, the
UI hides or explains the new actions, and the engine keeps its 501s.

## 2. rungv facts this design relies on

From `facts-rungv.md` (VERIFIED there unless marked), plus what this design read in the v0.20
source (`gvx-v020/cmd/rungv`, `pkg/gvx`):

- `run`/`restore` take `-S`/`-control-socket SPEC` (one flag; v0.19 has `-control-socket` too)
  and `--control-token-file PATH`. One socket serves gvx-ctrl/2 **and** the legacy checkpoint
  protocol (first byte `{`), so `rungv checkpoint -control-socket` keeps working on it.
- Without a token, a POSIX Unix socket authenticates the peer's UID (0600 socket). Windows Unix
  sockets and loopback TCP need a token. Client commands take `-S`, `--token-file`, `--timeout`.
- Socket parent dir: owned by euid, not group/world-writable, not a symlink (sticky `/tmp` is
  fine). Under lockdown rungv **cannot unlink its socket** (and token) on exit; the next `Listen`
  removes a stale one. sun_path: 104 bytes on macOS (rungv keeps itself under 100).
- Errors print `rungv <cmd>: CODE: message` on stderr. Exit 126 for UNAUTHORIZED,
  FORBIDDEN_BY_POLICY, LOCKDOWN_DENIED; 1 otherwise; 2 for usage. Codes: INVALID_ARGUMENT,
  UNSUPPORTED, ALREADY_PAUSED, NOT_PAUSED, NOT_FOUND, ALREADY_EXISTS, BUSY, LIMIT_EXCEEDED,
  EXEC_ACTIVE, SANDBOX_EXITING, INTERNAL. A dead sandbox: `connect: connection refused` or
  `no such file or directory`, exit 1.
- **exec** (`cmd/rungv/exec.go`, `pkg/gvx/control_exec.go`): the guest's exit code is rungv's.
  `-i` forwards stdin (else closed). `-t`: guest PTY; stderr merged into stdout; the initial size
  is `HostTerminalSize(stdin, stdout)` (24x80 when stdin is not a terminal); **resize comes only
  from SIGWINCH on the rungv client** while its stdin is a terminal; raw mode is entered by rungv
  when `-t -i` and stdin is a terminal. `--detach-keys` defaults to `ctrl-p,ctrl-q` (empty: off).
  `-d` prints the guest PID (not with -i/-t). `--pid-file` writes the PID once started (also
  without -d). `--privileged` defaults to true. SIGINT to the client → `exec.signal SIGINT` to
  the guest, exit code or 130. **The exec's environment is the sandbox's env merged with `-e`**
  (`mergeExecEnv(s.cfg.Envv, req.Env)`), and **its default user is the run's user**
  (`s.cfg.User`). **When the client disconnects, the guest command keeps running** detached, its
  output discarded. Paused: `ALREADY_PAUSED`. Default limit 32 exec sessions, 64 connections
  (LIMIT_EXCEEDED). Round trip ≈ 17 ms.
- **cp**: `cp :PATH -` writes a tar whose first entry is `base(PATH)`; `cp - :DIR` extracts a tar
  from stdin into DIR; `-a`, `-L`, `--no-overwrite-dir`, `--user`. Missing path: NOT_FOUND.
  There is no stat command (cp stats internally). **export** `-o - [--upper-only] [PATH]`.
- **pause** (`--quiesce-net=true`), **unpause**: silent. **kill** `-s SIG [--pid N|--all]`
  (default SIGKILL to PID 1). **stop** `-s SIG -t DUR` (SIGTERM, 10 s, then SIGKILL; the sandbox
  ends with its first process). SIGINT to `rungv run` tears the sandbox down at once (today's
  Stop).
- **stats** `-f json [--no-stream] [--interval]`: NDJSON `{timestamp, cpu{user_ns,sys_ns,...},
  memory{total_bytes,anon_bytes,file_bytes,tmpfs_bytes,limit_bytes}, network[{interface,...}],
  block_io[{disk,read_sectors,write_sectors,...}], pids, pids_limit}`. **top** `-f json`: one
  ProcessInfo per line `{pid,ppid,uid,gid,state,threads,cpu_ns,rss_bytes,comm,cmdline}`.
  **status** `-f json`: `{id,state,platform,lockdown_applied,resources{...},memory_used_bytes,
  ports[...],active_exec_sessions,pid_count,capabilities,started_at,...}`.
- **update** `-m`, `--force-memory`, `--cpus` (or `--cpu-quota`/`--cpu-period`), `--pids-limit`,
  `--isolate-lan=BOOL`, `--tune`, `-f json` → resource snapshot. Memory below current use fails
  without `--force-memory`. Guest `/proc/meminfo` follows at once. `run` takes `-pids-limit`.
- **port add/rm** need `--dynamic-ports` (or `--auto-publish`) on run; loopback binds only.
  **port-forward** needs nothing. **mount serve HOST:GUEST** works locked down and must stay
  running as a client process; it makes checkpoint answer BUSY. **events -f json**: `{timestamp,
  type, action, attributes}`; exec exits carry `pid` and `exit_code`.
- **Detection**: `rungv run -help` lists `-control-token-file` and `-dynamic-ports` only on
  v0.20 (`HasRunFlag`, cached per binary path). `ping -f json` gives a running sandbox's
  `capabilities`.

To verify in W1's integration tests (not yet run by anyone; adjust the design if wrong, and tell
the orchestrator): exec while a sandbox is paused (ALREADY_PAUSED, assumed), cp/stats/top while
paused, `rungv stop` and host SIGINT on a paused sandbox, the exit code of a sandbox whose PID 1
got a guest SIGKILL (assumed 137), `--privileged=true` exec in a `--privileged=false` sandbox,
a socket path containing a space (macOS "Application Support").

## 3. `internal/rungv` (W1): the runtime commands

New file `control.go` (skeleton in the tree). Every method takes a `Target{Socket, TokenFile}`
and builds `rungv <cmd> -S <socket> [--token-file F] ...`; none takes `-cache-dir` (they are not
in `cacheFlagCommands`, which is right). Errors are `*rungv.Error` as `output` makes them.

| Method | CLI | Notes |
|---|---|---|
| `Features(ctx) Features` | `run -help` (HasRunFlag cache) | `Control` = `-control-token-file`; `DynamicPorts` = `-dynamic-ports`. |
| `ForgetFeatures(path)` | – | drop the cache entry; `Install` (release.go) and the source build (source.go) call it after replacing a binary, so an in-app upgrade from v0.19 is seen without restarting the app. |
| `ExecArgs(t, o) []string` / `ExecCommand` | `exec -S s [--token-file f] [-i] [-t] [--detach-keys=] [-d] [-u U] [-w D] [-e K=V]... [--privileged=B] [--pid-file P] -- argv...` | `--detach-keys=` whenever `-i -t`. Bare `KEY` env entries are dropped (they would copy the app's env). |
| `CopyFrom(ctx,t,path,o) (io.ReadCloser, error)` | `cp [-a] [-L] -S s :PATH -` | stdout is the tar; `Close` kills/waits and returns the `*Error`. A NOT_FOUND before the first byte is returned by `CopyFrom` itself (wait for the first read or exit). |
| `CopyTo(ctx,t,dir,r,o) error` | `cp [-a] [--no-overwrite-dir] [--user U] -S s - :DIR` | r on stdin. |
| `StatPath(ctx,t,path) (PathStat, error)` | `cp -S s :PATH -` | read the first tar header, then kill the copy. Mode from `hdr.FileInfo().Mode()`. |
| `Export(ctx,t,path,upperOnly)` | `export -S s -o - [--upper-only] [PATH]` | P2 (changes, Files view). |
| `Pause` / `Unpause` | `pause` / `unpause` | |
| `Kill(ctx,t,KillOptions)` | `kill [-s SIG] [--pid N] [--all]` | |
| `StopGuest(ctx,t,sig,grace)` | `stop -s SIG -t DUR` | ctx deadline > grace + 5 s. |
| `Ping(ctx,t,wait)` | `ping -f json [--wait D]` | |
| `Status(ctx,t)` | `status -f json` | |
| `Stats(ctx,t)` / `StreamStats(ctx,t,iv,fn)` | `stats -f json --no-stream` / `stats -f json --interval iv` | NDJSON decoder; StreamStats returns when ctx ends or rungv exits. |
| `Top(ctx,t)` | `top -f json` | NDJSON. |
| `Update(ctx,t,Update) (Resources, error)` | `update [-m] [--force-memory] [--cpus] [--cpu-quota --cpu-period] [--pids-limit] [--isolate-lan=B] -f json` | refuse a zero Update locally. |
| `Events(ctx,t,fn)` | `events -f json` | P2. |
| `ControlCode(err) string`, `IsUnreachable(err) bool` | – | parse the **last** stderr line `rungv <cmd>: CODE: msg`; unreachable = `connection refused` / `no such file or directory` / `connect:`. |

`Checkpoint` keeps its signature for v0.19; W1 adds the token (a `Target`) for v0.20 inside
`rungv.go` (W1's file) as it sees fit. It is not a cross-workstream seam.

## 4. `internal/sandbox` (W1): a control socket for every run

### 4.1 Paths, socket location, token

`Supervisor.pathsFor(spec)` decides per launch, after resolving `client.Features(ctx)`:

| rungv | Spec.Checkpoint | ControlSocket | CheckpointDir | TokenFile | Control |
|---|---|---|---|---|---|
| v0.19 | false | – | – | – | false (today) |
| v0.19 | true | today's (`127.0.0.1:0` on Windows) | yes | – | false (today) |
| v0.20 | false | yes | – | Windows only | **true** |
| v0.20 | true | yes | yes | Windows only | **true** |

- **Socket path**: `<DataDir>/run/<sockName(id)>.sock`, where `sockName` is the ID when it has at
  most 16 characters (the user's 8-hex IDs), else the first 16 hex digits of `sha256(id)` (engine
  container IDs are 64 hex: today's fallback `$TMPDIR/gvxd/<64hex>.sock` is ~123 bytes on macOS
  and **cannot bind**; this fixes it). When the path is longer than `maxSocketPath` (100):
  `/tmp/gvxd-<euid>/<sockName>.sock` on Unix, created 0700 and checked with `Lstat` (a directory,
  ours, mode 0700, not a symlink; otherwise fail the start with an error naming it), and
  `%TEMP%\gvxd\<sockName>.sock` on Windows. On Windows v0.20 it is a **Unix socket path**, not
  `127.0.0.1:0`.
- **No `--name`/`--control`**: names could collide with the user's own rungv use. (`--name
  <spec.Name>` stays as today: it is the guest container ID, and without `--control` makes no
  socket.)
- **Token**: `TokenFile = <socket>.token` on Windows (v0.20); `--control-token-file` on run and
  restore, `--token-file` on every client command via `Target`. POSIX sockets get none (peer UID
  check); a later switch to tokens everywhere is one line.
- `Spec.flags` passes `-control-socket <sock>` (accepted by both versions; same flag as `-S`) and
  `--control-token-file <f>` when TokenFile is set. `RestoreArgs` does the same (restore shares
  run's sandbox flags in v0.20). Flags are passed only with Features.Control, so a v0.19 sees
  exactly today's argv (unit-tested).
- **Cleanup** in `exited`: remove the socket **and** the token (lockdown leaves both). Today's
  check `!strings.Contains(sock, ":")` would skip `C:\...` on Windows: keep a `tcp` flag in the
  proc instead. Never remove a path that is not a socket or token file (Lstat first).
- `State.Control = paths.Control` for the run's life. Ping is not needed at start: the socket
  is bound before the guest runs. `Supervisor.Control(id)` checks Status.Active && Control.
- The binary may be upgraded while a sandbox runs: its run keeps the socket of the version it
  started with. A run started by v0.19 stays without control until restarted (the UI says so).

### 4.2 Control (the `Control` interface, `control.go`)

`Supervisor.Control(id)` returns a value bound to the active run (its Target and the client of
the moment). Methods are thin wrappers of §3, except:

- **Exec** (`Control.Exec(ctx, ExecOptions) (ExecProcess, error)`): fails with `ErrPaused` when
  `State.Paused`. Always adds `--pid-file <DataDir>/run/<sockName>-<rand>.pid` (removed after
  Wait); a goroutine polls it (20 ms, until exit) to set `PID()` and close `Started()`.
  - **TTY on Unix**: `rungv exec -t [-i]` on a PTY of ours, the slave as stdin+stdout and
    controlling terminal (`Setsid`+`Setctty`, as `openPTY` does for run), **set to raw mode and
    sized (Rows/Cols, default 24x80) before Start** so rungv reads the size and no byte written
    early is echoed or cooked. stderr stays a pipe: only rungv's own messages come there; they go
    into Wait's error. `Resize` = `TIOCSWINSZ` on the master → SIGWINCH → rungv sends
    `exec.resize`. Stdin = the master; `Close` writes EOT. EIO on the master = EOF.
  - **TTY on Windows**: a pseudoconsole of the supervisor's (`pty_windows.go`), sized before
    start; `Resize` resizes it, and rungv follows the console's size. (The UI's terminal does
    not use this path: §6.1.)
  - **No TTY**: pipes. `Stdin()` nil without Interactive. Stdout and Stderr apart; Stderr also
    carries rungv's own last line on failure.
  - **Wait**: `(code, nil)` when rungv exited after the command started and the last stderr line
    is not a `rungv exec: ` error; `(-1, *rungv.Error)` when it failed before start (PID()==0) or
    with a `rungv exec: ` line after (the run ended under it). Exit code 1 with a guest message
    that imitates rungv's is misread; accepted.
  - **Signal**: `rungv kill --pid <PID()> -s SIG`; an error before Started.
  - **Close**: SIGTERM then SIGKILL (1 s) to the client; the guest command keeps running (rungv
    detaches it), as with docker. Release PTY/pipes; idempotent.
- **Kill**: `rungv kill`; a guest SIGKILL of PID 1 ends the run as Failed (exit code 137,
  verify), not Stopped.

### 4.3 Supervisor methods that change state

Rule: **never run rungv while holding `s.mu`** (subprocesses are slow); notify after unlocking.

- `Pause(ctx,id)` / `Unpause(ctx,id)`: rungv pause/unpause, then `State.Paused` and
  `EventChanged`. ALREADY_PAUSED / NOT_PAUSED make the state true/false and return nil. Paused
  clears at exit.
- `Update(ctx,id,u)`: rungv update; on success copy Memory (`u.Memory`), CPUs, IsolateLAN into
  `p.spec`, so `ActiveSpec` shows what runs; log a System line `── Updated: memory 256m, cpus 1.5`.
- `Shutdown(id, sig, timeout)` (docker stop): mark stopping/Stopping as `Stop` does; unpause
  when paused; `StopGuest(sig, timeout)` in a goroutine; wait for `done` up to `timeout + 5 s`;
  then today's path (SIGINT, then Kill after 5 s). An unreachable socket or an error → today's
  path at once. Without Control it **is** `Stop`. The skeleton's stub already falls back to Stop.
- `Stop` (SIGINT, today) is unchanged and stays the user sandboxes' Stop: see decision 4 (§11).
- `Checkpoint`: unchanged, plus the token; refuse while Paused ("Resume the sandbox first").
- `LiveUpdate(running, edited) (rungv.Update, bool)`: pure; live only when the only differences
  are Memory (to a non-empty value), CPUs ("" → "0", unlimited) and IsolateLAN, ignoring
  AutoStart and CreatedAt. Every other difference (ports, mounts, image, env, name, network,
  user, workdir, extra args, ...) is not live.

### 4.4 Events (P2)

With Control, the supervisor may run one `rungv events -f json` per run (started after launch,
ended at exit) and use it for: `lifecycle pause/unpause` (State.Paused when someone else paused
it), `port add/remove` (State.Ports, including removals the regex never saw), `oom` (an Advice).
v0.19 keeps the stderr regexes; with v0.20 the regexes stay too (the `publishing` line still
comes) and events only add.

## 5. Sandbox engine (W2)

### 5.1 Runner seams

`Runner` gains (frozen, in `runner.go`): `Features()`, `Control(id) sandbox.Control` (nil without),
`Pause`, `Unpause`, `Update`, `Shutdown`. `rungvRunner` delegates to the Supervisor
(`runner_proc.go`, written in the skeleton). The fake answers as a v0.19 (`Features{}`, nil
Control, `ErrNoControl`, Shutdown = Stop); W2 extends it with a `features` field, fake Controls
by ID, and Paused state. `Engine.Features()` and `Engine.CanControl(ref)` are the UI's seams.

**v0.19 rule**: every new handler first checks `e.cfg.Runner.Features().Control`; when false it
answers exactly today's 501 (`notSupportedMessage(...)` with today's `what`), **before** looking
the container up, so `router_test` and the v0.19 harness keep passing. With Features.Control but
a running container whose `Control(id)` is nil (it started before the upgrade): 409 `Container
<id> was started without a control socket; restart it to use <what>`.

Common checks (docker's messages): not running → 409 `Container <id> is not running`; paused →
409 `Container <id> is paused, unpause the container before exec` (exec) / `Container <id> is
paused` (others that refuse); restarting → 409 `Container <id> is restarting, wait until the
container is running`.

### 5.2 exec

New `exec.go` (+ tests). Records in memory: `execRecord{ID (64 hex), ContainerID, Config (the
create body), OpenStdin/Stdout/Stderr, Running, ExitCode, Pid, CanRemove, proc}`; listed as
`ExecIDs` in container inspect; dropped when the container is removed, and when the container's
run ends plus 5 minutes after the exec exited (docker GCs them too).

- `POST /containers/{id}/exec` `{AttachStdin, AttachStdout, AttachStderr, DetachKeys, Tty,
  ConsoleSize, Env, Cmd, Privileged, User, WorkingDir}` → 201 `{Id}`. Empty Cmd → 400 `No exec
  command specified`. Emits `exec_create: <cmd>`.
- `POST /exec/{id}/start` `{Detach, Tty, ConsoleSize:[h,w]}`:
  - Options: `Argv = Cmd`; `Env = container Config.Env + request Env` (bare `KEY` dropped), **plus
    `GVX_NET=` for a container started with netinit** (the exec env inherits the spec's env,
    which carries the WireGuard private key); `User = request User || Config.User` (**always
    explicit**: a netinit container's spec runs as root, its user is applied by netinit);
    `Workdir = request || Config.WorkingDir`; `Privileged = &request.Privileged` (docker's
    default is unprivileged; rungv's is privileged); `Interactive = AttachStdin`; `TTY = Tty`;
    Rows/Cols from ConsoleSize.
  - **Detach**: 200 with no body; start the same exec **without `-d`** in a goroutine, draining
    and discarding its output, so Running/ExitCode stay accurate (rungv `-d` would lose them).
  - Otherwise hijack exactly as `containerAttach` does (factor the head/content-type part into a
    helper used by both): `101 UPGRADED` with `Upgrade: tcp`, else `200 OK`; raw-stream for Tty,
    else multiplexed-stream (≥ 1.42) / raw-stream with frames. Stdout → frames 1, Stderr → 2
    (raw bytes for Tty). Client stdin → `Stdin()`; EOF from the client → `Stdin().Close()`.
    Detach keys (request's, else ctrl-p ctrl-q) on Tty: detach the client (Close the hijack
    only; the exec keeps running, its output drained). Client gone without detach: `Close()`
    the exec client (the guest command keeps running, as on docker). At exit: write what remains,
    CloseWrite, close.
  - Exit codes: `Wait` `(code, nil)` → code. Failed before start (`PID()==0`): write the message
    on stderr (frame 2, or raw for Tty) as `OCI runtime exec failed: <rungv message>` and set
    127 when the message says the executable was not found, else 126 (docker's codes);
    ALREADY_PAUSED → 409 before the hijack (check State.Paused first; the race is the 126 path).
    Failed after start (the run ended) → 137. LIMIT_EXCEEDED → 126 with the message.
  - Emits `exec_start: <cmd>` and `exec_die` (attributes `execID`, `exitCode`).
- `POST /exec/{id}/resize?h&w` → `Resize`; 200 when the exec has no terminal; 404 unknown;
  409 not running. On Windows a resize error is ignored (200).
- `GET /exec/{id}/json` → `{ID, Running, ExitCode (null while running), ProcessConfig{tty,
  entrypoint, arguments, privileged, user}, OpenStdin, OpenStderr, OpenStdout, CanRemove,
  ContainerID, DetachKeys, Pid}`.
- Concurrency: one mutex for the exec map; no runner call under it. Each exec is its own process;
  rungv's 32-session limit answers LIMIT_EXCEEDED.
- The app's own Terminal tab (`dockerapi.Client.Exec` + `ExecDetached` sweep) must work against
  it: test with `dockerapi` against the httptest engine.

### 5.3 cp (`archive.go`)

- Not running → 501 `Copying files to and from a container that is not running` (the engine
  cannot read a stopped sandbox's filesystem). Paused → 409.
- `HEAD /containers/{id}/archive?path=P` → `StatPath`; 200 with
  `X-Docker-Container-Path-Stat: base64(JSON{name,size,mode,mtime,linkTarget})`; NOT_FOUND →
  404 `Could not find the file P in container <name>`.
- `GET` → the same header, then `application/x-tar` from `CopyFrom` (relative paths are relative
  to `/`, as docker resolves against the root).
- `PUT ?path=DIR&noOverwriteDirNonDir=&copyUIDGID=` → `StatPath(DIR)`: 404 when missing, 400
  `extraction point is not a directory` when not a dir; the body may be gzip-compressed (sniff
  the magic, decompress); bzip2/xz → 400. `CopyTo` with `Archive = copyUIDGID`,
  `NoOverwriteDir = noOverwriteDirNonDir`. A "read-only file system" message → 403 `container
  rootfs is marked read-only`. 200 on success.

### 5.4 pause, unpause, top, update, kill, stop (`container_runtime.go`)

- **pause/unpause** → `Runner.Pause/Unpause`; 409 `Container <id> is already paused` / `is not
  paused`; emit `pause`/`unpause`. Status word `paused` when `State(id).Paused` (list, inspect
  `State.Paused: true`, `Status: "paused"`, ps `Up 3 minutes (Paused)`); `info.ContainersPaused`
  counts them. Stop/restart/rm -f of a paused container go through Shutdown (which unpauses);
  kill of a paused one: unpause, then kill.
- **top** `GET /containers/{id}/top?ps_args=` → `{Titles:["UID","PID","PPID","C","STIME","TTY",
  "TIME","CMD"], Processes:[[uid, pid, ppid, "0", "-", "?", hh:mm:ss of cpu_ns, cmdline joined
  or "[comm]"]]}`. ps_args are ignored.
- **update** `POST /containers/{id}/update` → `{Warnings:[]}`. Fields: `Memory` (≥ 6 MiB, else
  400 `Minimum memory limit allowed is 6MB`), `NanoCpus`, `CpuQuota`+`CpuPeriod`, `PidsLimit`,
  `RestartPolicy` (AutoRemove conflict → 400 as at create). They go into the record's
  HostConfig and Spec (Memory `<n>b`, CPUs, and `-pids-limit` in ExtraArgs at the next start when
  `HasRunFlag("pids-limit")`) and
  are saved; when running with Control, `Runner.Update`. Running without Control (v0.19, or an
  old run) → Warning `The new limits apply at the next start: the container runs without a
  control socket`. Other non-zero fields (CpuShares, BlkioWeight, Cpuset*, MemorySwap,
  MemoryReservation, KernelMemory, ...) → Warning `<Field> is not supported and was ignored`.
  Emits `update`.
- **kill** (`runner_proc.go`'s Signal): with Control, `Kill{Signal: name}` to PID 1, so the
  container's process sees the signal as on docker (a SIGTERM-ignoring PID 1 ignores it);
  an unreachable socket falls back to today's host signal. Without Control: today's path.
- **stop/restart**: `Runner.Shutdown(id, sig, t)` with `sig = ?signal= || Config.StopSignal ||
  SIGTERM` and `t = ?t= || Config.StopTimeout || 10 s` (`t < 0`: 24 h). `stopContainers` (Close)
  and `StopRunning` use Shutdown too. The record still ends `exited` with the exit code of the
  first process (143 for a TERM handler that exits by the signal, 137 after the SIGKILL).

### 5.5 stats

`streamSampleStats` takes `Control.Stats` (one-shot) or one `StreamStats` per streaming request
(`stream=1`), and maps: `cpu_stats.cpu_usage.total_usage = user_ns + sys_ns`;
`system_cpu_usage` and `online_cpus` as today (wall time × NumCPU); `memory_stats{usage:
total_bytes, limit: limit_bytes (0 → host memory), stats{anon, file, shmem: tmpfs_bytes,
inactive_file: 0}}`; `networks` per interface except `lo`; `blkio_stats.io_service_bytes_recursive`
`[{major:254, minor:i, op:"read"|"write", value: sectors×512}]`; `pids_stats{current: pids,
limit: pids_limit when > 0}`. Without Control or on an error: today's procstat sampling.

### 5.6 `docker network connect` on a running container

Today the hub and names follow at once, but the guest link keeps its addresses until the next
start. With Control, for a container whose **sandbox started with netinit and a link** (it joined
the hub at start):

1. `connectNetwork` records the address as today; then `Fabric.SetAddrs(id, addrs)` (new, W2,
   `internal/overlay/fabric.go`) sets the hub peer's allowed IPs and returns the guest config with
   the **same key** (the peer keeps its private key) and the new addresses and routes.
2. `Control.Exec{Argv: [guestcfg.Path, "--gvx-apply"], Env: ["GVX_NET=<encoded>"], User: "0",
   Privileged: &true}`, waited for with a 10 s timeout.
3. netinit's apply mode (`internal/overlay/netinit/apply.go`, new; `guestcfg.ApplyArg =
   "--gvx-apply"`): when `argv[1] == ApplyArg` and GVX_NET is set, reconcile `gvx0` with the
   config (create it when missing; add missing addresses, remove extra ones; peer allowed IPs =
   addresses + routes; add routes), touch nothing else (hosts, isolation, user), exit 0, or 1
   with the reason on stderr.
4. Failure → `Config.Logf` and today's behavior (the next start applies it). The API answer is
   200 either way, as today. Disconnect does the same with the address removed.

Not live: a container that started without a link (bridge only; it has no netinit mount and no
`-host-dns`, so it would get addresses but no names), and isolation changes (internal ↔ public).
Both apply at the next start, as today.

### 5.7 Elsewhere in the engine

- `/info` Warning with Features.Control: `GVX Desktop's experimental sandbox engine: commit,
  export and diff are not supported yet.`; without: today's line.
- Still 501: `/changes`, `/export` (P2 via `Export`), `/commit`, `/attach/ws`.

## 6. App and UI (W3)

Feature detection in the UI: `rungv.Client.Features` runs `rungv run -help` (5 s worst case, then
cached): compute it **asynchronously** whenever the rungv path changes or an install finishes, into
a signal (`m.RungvFeatures`), never on the UI thread. Per sandbox: `Row.State.Control` and
`State.Paused`. Per container in engine mode: `m.engine.CanControl(id)` and `m.engine.Features()`.

### 6.1 Sandboxes: Terminal tab

- Append `ui.Tab(tr("Terminal"), ...)` as tab 4 (`sandboxTerminalTab = 4`; existing tab indexes
  stay). It reuses `terminalTab`/`shellView` with a `m.sandboxShell()` target built like
  `containerShell`:
  - not active → `The sandbox is not running` / `Start it to open a shell in it.` (existing keys);
  - Paused → `The sandbox is paused` / `Resume it to open a shell in it.`;
  - `!State.Control` and features have Control → `Restart the sandbox to open a shell` / `It
    started before rungv was updated. Restart it to open a shell, pause it, or change its
    resources while it runs.`;
  - `!State.Control` and no Control feature → `Needs rungv v0.20 or later` / `Update rungv in
    Settings to open a shell in a running sandbox, pause it, and change its resources while it
    runs.`;
  - Offline (render) → `cannedShell(containerTranscript(name))`;
  - else `Start = func(cols, rows)`: `ctl, ok := m.sup.Control(id)`; `argv :=
    ctl.ExecArgv(rungv.ExecOptions{Argv: term.ShellCommand(mark), Interactive: true, TTY:
    true})`; `term.Start(argv, term.Env(withoutOverrides(os.Environ())), cols, rows)` (the same
    PTY/ConPTY path as kubectl exec, so resize works on every OS), wrapped in `term.Swept` whose
    sweep runs `ctl.Exec` of `term.SweepCommand(mark)` with Detach (the guest shell survives a
    client close, so the sweep matters). `Key = id + "@" + StartedAt` so a restart reopens it.
- The shell runs as the sandbox's user (rungv's default).

### 6.2 Containers: Terminal tab in sandbox-engine mode

`containerShell` drops its blanket "not supported" message. In engine mode:
`!m.engine.Features().Control` → `Needs rungv v0.20 or later` / `Container exec in the
experimental sandbox engine needs rungv v0.20 or later. Update rungv in Settings, then restart
the container.`; running but `!CanControl(id)` → `Restart the container to open a shell` / `It
started before rungv was updated to v0.20.`; paused (`State == "paused"`, either mode) → `The
container is paused` / `Resume it to open a shell in it.`; else today's `dockerShell` through the
engine. Offline → canned shell (the `containers-sandbox-engine` shot then shows a shell).

### 6.3 Pause and Resume

- Sandboxes: in `detailHeader`, when active, Control and not Paused: `Pause` (lucide.Pause,
  outline) next to Stop/Restart; when Paused: `Resume` (lucide.Play, primary) and Stop. The same
  two entries in the row menu. A `Paused` badge (existing key) in the list row and header meta.
  Snapshot buttons are disabled while paused. `m.PauseSandbox(id)` / `m.ResumeSandbox(id)` run
  async and toast errors: `Could not pause %s` / `Could not resume %s`.
- Containers: `Pause` / `Resume` menu items for running / paused containers, in dind mode always
  and in engine mode when `CanControl`. `internal/dockerapi` gains `Pause(ctx,id)` and
  `Unpause(ctx,id)` (W3).

### 6.4 Editor: live changes

When the draft's sandbox runs with Control: `u, live := sandbox.LiveUpdate(active, draft)` where
`active` is `m.sup.ActiveSpec(id)`.

- Caption (replaces `Ports and mounts are opened before the guest starts and stay fixed while it
  runs.` in that case): `Memory, CPUs and LAN isolation apply to a running sandbox at once. Ports,
  mounts and the other settings take a restart.`
- `live && !u.IsZero()`: the primary button is `Save and apply`; Save and Save-and-apply both
  save, then `m.sup.Update(ctx, id, u)` async. Success → toast `Applied to %s` / `The running
  sandbox has the new memory, CPU and LAN settings.`; failure → `toastError(tr("Could not change
  the running sandbox"), err)` (the spec stays saved; the next start uses it), no "restart" toast.
- `!live`: today's `Save and restart` and today's toast.
- Without Control: exactly today.

### 6.5 Usage

`LoadSandboxUsage`: for a row with `State.Control`, take `ctl.Stats(ctx)` (2 s timeout, rows in
parallel) in place of procstat: CPU% = Δ(user_ns+sys_ns) / Δtimestamp × 100 (100 per busy core,
the scale `procstat.CPUPercent` uses; check it), Memory = `total_bytes`, Limit = `limit_bytes`
(else `memoryLimit(spec.Memory)`). Keep the previous sample per row (as `sandboxSample`, with a
guest variant). An error falls back to procstat for that row. Container usage needs no change: it
reads the engine's stats (§5.5).

### 6.6 Settings → Docker → Experimental

With features Control, the two notes become `Works: docker run (-d, -i, -t, --rm), exec, cp,
pause, top, update, stats, build (BuildKit and Compose), ps, logs, stop, start, rm, ports on
127.0.0.1, volumes, bind mounts, pull/images, networks between containers, and Compose with
several services.` and `Not yet supported: commit, export and diff.` Without:
today's two notes plus `Update rungv to v0.20 or later for exec, cp, pause, top and update.`

### 6.7 New UI strings (exact English keys; all six catalogs)

```
The sandbox is paused
Resume it to open a shell in it.
Restart the sandbox to open a shell
It started before rungv was updated. Restart it to open a shell, pause it, or change its resources while it runs.
Needs rungv v0.20 or later
Update rungv in Settings to open a shell in a running sandbox, pause it, and change its resources while it runs.
Container exec in the experimental sandbox engine needs rungv v0.20 or later. Update rungv in Settings, then restart the container.
Restart the container to open a shell
It started before rungv was updated to v0.20.
The container is paused
Pause
Resume
Could not pause %s
Could not resume %s
Memory, CPUs and LAN isolation apply to a running sandbox at once. Ports, mounts and the other settings take a restart.
Save and apply
Applied to %s
The running sandbox has the new memory, CPU and LAN settings.
Could not change the running sandbox
Works: docker run (-d, -i, -t, --rm), exec, cp, pause, top, update, stats, build (BuildKit and Compose), ps, logs, stop, start, rm, ports on 127.0.0.1, volumes, bind mounts, pull/images, networks between containers, and Compose with several services.
Not yet supported: commit, export and diff.
Update rungv to v0.20 or later for exec, cp, pause, top and update.
```

Existing keys reused: `Terminal`, `Paused`, `The sandbox is not running`, `Start it to open a
shell in it.`, `The container is not running`. API messages to the docker CLI stay English.

### 6.8 Render shots (`render.go`)

Demo rows get `State.Control = true` (one running row `Paused`). New shots: `sandboxes-terminal`
(canned shell), `sandboxes-paused` (Resume in the header, the badge), `editor-live` (the new
caption and `Save and apply`); `containers-sandbox-engine` now shows the canned shell. Check en
and ko PNGs.

## 7. Edge cases

| Case | Behavior |
|---|---|
| v0.19 rungv | No new flag (unit-tested argv). Control false everywhere. Engine 501s with today's messages; UI explains (§6). |
| Run started by v0.19, binary upgraded since | State.Control false for that run; UI: "Restart ... "; engine: 409 "restart it to use ...". `ForgetFeatures` after install so new runs get a socket. |
| Paused sandbox | Exec/cp → ErrPaused / 409 (pre-checked; the race returns ALREADY_PAUSED → 409/126). Stop: Shutdown unpauses first; Stop (SIGINT) works (verify). Checkpoint refused. Usage keeps sampling (stats while paused: verify; on error procstat). |
| Sandbox exits during exec | rungv exec fails after start → `Wait` err → exit 137 in the engine; the UI terminal ends as any program does. The socket is gone: further calls `IsUnreachable`. |
| Sandbox exits during cp/stats | the command fails; GET archive ends early (the client sees a short tar); stats stream ends; engine stats ends its stream like today at exit. |
| Concurrent execs | independent processes; the engine's exec map has its own mutex; rungv limits 32 sessions → LIMIT_EXCEEDED → 126 with the message. |
| Client disconnects mid-exec | the guest command keeps running (rungv detaches it), as on docker; the UI's terminal sweeps its marked processes. |
| Windows token | `<sock>.token`, `--control-token-file` on run/restore and `--token-file` on each client command; UNAUTHORIZED (126) means a mismatch: logged with the path. Removed at exit. |
| sun_path | ≤ 100 bytes by construction (§4.1): hashed names for long IDs, `/tmp/gvxd-<euid>` fallback (checked ours, 0700, no symlink). Unit test with a 64-hex ID and a long DataDir. |
| Lockdown | rungv can't unlink its socket/token: the supervisor removes them at exit; a stale one is replaced by rungv's own probe at the next run. `port add` without `--dynamic-ports` → FORBIDDEN_BY_POLICY (P2 only). Runtime bind mounts are LOCKDOWN_DENIED: never used. |
| GVX_NET in exec env | the engine passes `GVX_NET=` so an exec does not see the overlay's private key. |
| User of an exec | the engine passes Config.User (netinit containers run their spec as root). |
| Update below current memory | rungv refuses (no `--force-memory` from the UI/engine); the error is shown / returned as 500 with rungv's message. |
| Name collisions with the user's rungv | none: explicit socket paths under our dirs; no `--control`/`-n`. |
| The user's real sandboxes | the UI acts on them only on the user's click (Terminal, Pause, live update), as with Stop today. Tests never do. |

## 8. P2 designs (build only if cheap; otherwise leave for later)

- **Dynamic ports** (W1 Client `Ports/AddPorts/RemovePorts` + W3): with Features.DynamicPorts,
  user sandboxes run with `--dynamic-ports`; the editor's port changes apply live (`port add` of
  new loopback specs, `port rm --force` of removed static ones); non-loopback host IPs still need
  a restart. Recipes with `-auto-publish` already allow `port add`. k3s ServiceLB (8080/8443)
  toggles via `port add/rm` when the cluster runs, while `--disable=servicelb` still needs a
  restart to take effect inside k3s, so the toggle stays restart-only unless only ports changed.
- **Files view** (W3 + W1's `StatPath`/`CopyFrom`/`Export`): the explorer gets a "running
  sandbox/container" source that lists a directory by `CopyFrom(dir)` read for headers only, one
  level, aborting after N entries (a deep directory tree streams fully: cap the bytes, show
  "partial"). Download = CopyFrom to a host tar/extract; upload = CopyTo. Engine
  `GET /containers/{id}/changes` from `Export(upperOnly)` headers (whiteouts → D, others → A; C
  for directories that exist below), `GET /export` from `Export("")`.
- **Recipe live changes**: shared folders via `rungv mount serve HOST:GUEST` = a long-lived client
  process per folder, owned by the supervisor (started after launch, restarted with backoff if it
  dies while the run lives, killed at exit; `mount rm` when the user removes the folder). It makes
  checkpoint answer BUSY (checkpoint UI must say so). Until built, shared folders stay
  restart-only.
- **Events** in place of regex (§4.4).

## 9. Test plan

All unit tests run in `GOWORK=off go test -short ./...` with no rungv. Integration and harness
tests use the TEST binary through `GVX_RUNGV=<scratchpad>/rungv020/rungv` (or
`rungv.Client{Path}`), a temp cache (`/tmp/gvxd-test-se/cache` for the engine harness, a temp dir
otherwise), `gvxd-*` names, ports 18090-18099/15xxx; they skip under `-short`, without a binary,
and (for v0.20 assertions) when `Features().Control` is false. They clean up their sandboxes,
sockets and temp dirs. A v0.19 regression run with `GVX_RUNGV=~/.gvx/bin/rungv` and a temp cache
is allowed (it only executes that binary).

- **W1 unit**: `ExecArgs` vectors (token, -i/-t/`--detach-keys=`, -d, -u/-w/-e, bare KEY dropped,
  privileged, pid-file, `--`); `RunArgs`/`RestoreArgs` with and without Control/TokenFile (v0.19
  argv byte-identical to today); `pathsFor` table of §4.1, a 64-hex ID under a long DataDir (≤ 100
  bytes, hashed), the `/tmp/gvxd-<euid>` checks (a symlink or a foreign dir fails); Windows paths
  by a GOOS-parameterized helper; `ControlCode`/`IsUnreachable` on captured stderr; `LiveUpdate`
  table; NDJSON decoding of captured stats/top/status (testdata); `Features` from a stub script
  printing a v0.19 and a v0.20 usage (unix); `ForgetFeatures`. Existing `supervisor_test.go`
  passes unchanged.
- **W1 integration** (`control_integration_test.go`): a supervisor in a temp DataDir (on macOS
  `t.TempDir()` is long: exercises the fallback) starts `gvxd-ctl-*` `alpine:3.20 sleep 300`:
  State.Control; non-TTY exec `sh -c 'echo out; echo err >&2; exit 42'` (streams apart, 42,
  PID > 0); `cat` with Interactive, Stdin().Close → EOF; TTY exec `stty size` after `Resize(30,
  100)` → `30 100`, raw input not echoed twice; exec of a missing binary → error before start;
  Pause → State.Paused + event, Exec → ErrPaused, Unpause; CopyTo then CopyFrom round trip;
  StatPath of a file and a dir; Stats/Top shapes; Update `-m 256m` → `/proc/meminfo` and
  ActiveSpec; Kill SIGTERM to `sh -c 'trap "exit 7" TERM; ...'` → Failed with 7 (or as rungv
  reports); Shutdown of `sleep` with timeout 2 s → ~2 s, Stopped; Shutdown while paused; socket and
  token gone after exit; the §2 "to verify" list, reported to the orchestrator.
- **W2 unit** (fake Control/ExecProcess in W2's own test files): exec create/start/inspect/resize
  over 101 and 200; mux framing (non-Tty) and raw (Tty); stdin half-close → Stdin().Close; detach
  keys; detached exec Running → exit code; exit-code mapping (0, 42, 126, 127, 137); paused/not
  running 409; v0.19 → today's 501 message (router_test unchanged); old run → 409; ExecIDs;
  exec events; `GVX_NET=` and explicit User in the options; **`dockerapi.Client.Exec` +
  `ExecDetached` against the httptest engine** (the UI's path). cp: HEAD header (base64 JSON),
  GET passthrough, PUT dir checks, gzip body, NOT_FOUND → 404, read-only → 403, stopped → 501.
  pause/unpause status words, ps "(Paused)", info count, events. top shape. update: record +
  Update call + warnings (unsupported fields, no control). kill: guest vs host fallback. stop:
  signal/timeout precedence into Shutdown. stats mapping from a canned `rungv.Stats`; procstat
  fallback. `Fabric.SetAddrs` keeps the key and updates allowed IPs (fabric_test); netinit apply's
  reconcile logic as pure functions (the netinit binary is linux-only).
- **W2 harness** (`GVX_ENGINE_HARNESS=1`, TEST binary): `docker exec gvxd-x echo ok`; `exec ...
  sh -c 'exit 3'` → 3; `echo hi | docker exec -i gvxd-x cat`; `docker exec -it` through a creack
  pty (`stty size`, resize, `exit 5`); `exec -d` then `exec ... ps`; `exec -u nobody id`; `docker cp`
  both ways and `docker cp gvxd-x:/etc/hostname -`; pause/unpause + ps; `docker top`; `docker update
  --memory 256m` then `exec cat /proc/meminfo`; `docker kill -s TERM` with a trap; `docker stop -t
  2` of `sleep` ≈ 2 s; `docker stats --no-stream` with non-zero memory; network connect live: two
  containers, connect a running one to the other's network, reach by name and by IP within seconds
  without a restart. **`system_e2e_test.go` steps 10 and `exec_and_build_501`**: when the
  runner's Features have Control, assert exec works; else today's 501 (build stays as is).
- **W3 unit**: pure gating functions (sandbox and container shell targets from row/state/
  features/offline; Pause/Resume visibility), the editor's button and caption choice from
  `LiveUpdate`, CPU% from two Stats samples, `dockerapi` Pause/Unpause against an httptest server,
  `TestCatalogsAreComplete`, `~/go/bin/ggui i18n check`, render shots (look at the PNGs, en and
  ko). No live rungv in UI tests.
- **Everyone before reporting done**: `gofmt -l`, `GOWORK=off go build ./... && GOWORK=off go vet
  ./... && GOWORK=off go test -short ./...`.

## 10. Work table, seams, coordination

Every file has exactly one owner. Create files only in your own packages. A change to another
owner's file goes to the orchestrator with the exact diff and why. Never call another
workstream's code while holding your own mutex.

| WS | Scope | Files (existing + new) |
|---|---|---|
| **W1** rungv + sandbox | §3, §4, P2 Client parts | `internal/rungv/*` (incl. new `control.go`, `control_test.go`, `testdata/`; `rungv.go` Checkpoint token + Features cache; `release.go`/`source.go` ForgetFeatures), `internal/sandbox/*` (incl. new `control.go`, `control_exec*.go`, `control_*_test.go`; `spec.go`, `supervisor.go`, `stdio.go`, `pty_unix.go`, `pty_windows.go`, optional `events.go`) |
| **W2** engine | §5 | `internal/engine/*` (all, incl. the formerly frozen `runner.go`, `engine.go`, `router.go`, `httpx.go`, `fake_test.go`, `harness_test.go`, `router_test.go`; `system_e2e_test.go`, `system_test.go`; new `exec.go`, `archive.go`, `container_runtime.go` + tests), `internal/overlay/*` (incl. `fabric.go` SetAddrs, `guestcfg/guestcfg.go` ApplyArg, new `netinit/apply.go`, `netinit/main.go`), `docs/design/sandbox-engine.md` (update §1, §11's table) |
| **W3** UI | §6 | `internal/ui/*` (all, incl. `i18n_{ko,ja,zh_hans,zh_hant,de,hi}.go`), `internal/dockerapi/*` (Pause/Unpause), `render.go`, `main.go` |

Nobody edits `internal/term`, `internal/procstat`, `internal/recipes`, `internal/store`,
`internal/dockerproxy` in this round (ask the orchestrator if needed; P2 recipe work would give
`internal/recipes` to W3). This document is the architect's; send corrections to the orchestrator.

### Frozen seams (signatures and doc comments fixed; bodies are the owner's)

W1 → W2, W3 (`internal/rungv/control.go`):
`ErrNotImplemented`; `Target{Socket, TokenFile}`, `Target.Valid`; `Features{Control,
DynamicPorts}`, `(*Client).Features(ctx) Features`, `ForgetFeatures(path)`;
`ExecOptions{Argv, Env, User, Workdir, Interactive, TTY, Detach, Privileged *bool, PIDFile}`,
`(*Client).ExecArgs(t, o) []string`, `(*Client).ExecCommand(ctx, t, o) *exec.Cmd`;
`CopyOptions{Archive, FollowLink, NoOverwriteDir, User}`, `(*Client).CopyFrom(ctx, t, path, o)
(io.ReadCloser, error)`, `(*Client).CopyTo(ctx, t, dir, r, o) error`, `PathStat{Name, Size, Mode,
Mtime, LinkTarget}`, `(*Client).StatPath(ctx, t, path) (PathStat, error)`, `(*Client).Export(ctx,
t, path, upperOnly) (io.ReadCloser, error)`; `(*Client).Pause/Unpause(ctx, t) error`;
`KillOptions{Signal, PID, All}`, `(*Client).Kill(ctx, t, o) error`, `(*Client).StopGuest(ctx, t,
signal, grace) error`; `Ping` (+`Has`), `(*Client).Ping(ctx, t, wait) (Ping, error)`;
`Resources`, `PortRule`, `Status`, `(*Client).Status(ctx, t)`; `Stats` (+`CPUNanos`),
`(*Client).Stats(ctx, t)`, `(*Client).StreamStats(ctx, t, interval, fn) error`; `Process`,
`(*Client).Top(ctx, t) ([]Process, error)`; `Update{Memory, ForceMemory, CPUs, CPUQuota,
CPUPeriod, PidsLimit *int64, IsolateLAN *bool}` (+`IsZero`), `(*Client).Update(ctx, t, u)
(Resources, error)`; `Event`, `(*Client).Events(ctx, t, fn) error`; `Code*` constants,
`ControlCode(err) string`, `IsUnreachable(err) bool`.

W1 → W2, W3 (`internal/sandbox`):
`ErrNoControl`, `ErrPaused`; `Control` interface (`Target`, `ExecArgv`, `Exec`, `CopyFrom`,
`CopyTo`, `StatPath`, `Export`, `Stats`, `StreamStats`, `Top`, `Kill`); `ExecOptions{rungv.ExecOptions;
Rows, Cols}`; `ExecProcess` interface (`Stdin`, `Stdout`, `Stderr`, `Resize`, `PID`, `Started`,
`Wait`, `Signal`, `Close`); `(*Supervisor).Control(id) (Control, bool)`, `Pause(ctx, id) error`,
`Unpause(ctx, id) error`, `Update(ctx, id, rungv.Update) (rungv.Resources, error)`,
`Shutdown(id, signal, timeout) error`; `LiveUpdate(running, edited Spec) (rungv.Update, bool)`;
`State.Control`, `State.Paused`; `Paths.TokenFile`, `Paths.Control`.

W2 → W3 (`internal/engine/engine.go`): `(*Engine).Features() rungv.Features`,
`(*Engine).CanControl(ref string) bool`.

W2-internal but frozen against its fake (`runner.go`): `Runner.Features`, `Control`, `Pause`,
`Unpause`, `Update`, `Shutdown`.

Until W1 lands, the stubs answer as a v0.19 (zero Features, no Control, `ErrNoControl`,
`ErrNotImplemented`; Shutdown = Stop), so the tree behaves exactly as today. W2 and W3 code against
fakes of `sandbox.Control`/`sandbox.ExecProcess` (and the fake Runner) and integrate as W1 reports.
Suggested W1 order: Features + pathsFor/flags + State.Control + `Supervisor.Control` + Exec
(unblocks the Terminal tabs and docker exec), then cp/StatPath/Stats/Top/Kill, then
Pause/Unpause/Update/Shutdown/LiveUpdate.

## 11. Decisions (refinements of the orchestrator's, with reasons)

1. **CLI subprocesses**, no gvx-ctrl/2 client (as decided). Exec costs ≈ 17 ms, fine.
2. **Explicit socket for every run with v0.20**, in the supervisor's run dir, merged with today's
   checkpoint socket; no `--name`/`--control`. Refined: the flag passed is `-control-socket`
   (the same flag as `-S` in v0.20, and the one v0.19 already gets), so there is one code path;
   socket names of long IDs are hashed, and the fallback dir is `/tmp/gvxd-<euid>` because
   today's `$TMPDIR/gvxd/<64hex>.sock` overflows sun_path on macOS (the engine's sockets would
   never bind).
3. **Tokens only where required** (Windows Unix sockets), passed consistently on run/restore and
   every client command. POSIX relies on rungv's peer-UID check.
4. **Graceful stop for engine containers only** (docker stop semantics: StopSignal, timeout,
   SIGKILL). User sandboxes keep today's immediate Stop (SIGINT): a graceful stop would make
   every sandbox whose PID 1 ignores SIGTERM (a plain `sleep`, many shells) wait the full stop
   timeout. Recipe sandboxes are opted in (§12.1): `Supervisor.Stop` of a recipe's run with a
   control socket is `Shutdown(id, "SIGTERM", grace)`.
5. **The UI's sandbox terminal runs `rungv exec -it` through `internal/term`** (`ExecArgv` +
   `term.Start`) rather than the supervisor's PTY: it is the code path kubectl exec already uses,
   with ConPTY on Windows, so resize works everywhere. The engine's docker exec uses the
   supervisor's PTY (`Control.Exec`), as docker run does today (pipes on Windows).
6. **Detached docker exec** runs a non-detached rungv exec in the background, so inspect's
   Running/ExitCode are right (rungv `-d` would lose the exit code).
7. **Live network connect only for containers that started on the overlay**; others at the next
   start (they lack netinit and `-host-dns`). Pre-wiring every container with netinit would change
   every plain `docker run`'s DNS path: not in this round.
8. **Per-run feature detection is `State.Control`** (set at launch from the binary's Features), not
   a ping per call; `ping` capabilities are W1's to use if a policy ever restricts a run.
9. P2 items are designed but not assigned as must-do; each owner may build one when P1 is green.

## 12. The Docker Engine recipe on v0.20 (follow-up)

Verified against docker:dind under rungv v0.20 by `TestDockerEngineHarness`
(`internal/sandbox/dind_harness_test.go`, `GVX_SANDBOX_HARNESS=1 GVX_DIND_HARNESS=1`). The
guest's PID 1 is `docker-init` (tini), which forwards signals to dockerd.

### 12.1 Graceful stop of recipe sandboxes

`Supervisor.StopGrace`, wired to `recipes.StopGrace` (as `Prepare` is to `recipes.Prepare`): a
sandbox with a `Recipe` (dockerd, k3s, the registry) stops as `docker stop`
stops a container: SIGTERM to PID 1 through the control socket, SIGKILL after the grace, then the
SIGINT path. The grace is the stop timeout; dockerd gets at least 20 s (its own
`shutdown-timeout` is 15 s for its containers, plus cleanup). Every caller of `Stop` (Stop,
Restart, Delete, Stop all, quitting) gets it. Measured: dockerd with one container ends in
0.4 s, logging `Processing signal 'terminated'` and `Daemon shutdown complete`. A run without a
control socket (v0.19), and user sandboxes, keep the SIGINT stop (decision 4).

### 12.2 daemon.json, reloaded on SIGHUP

- The engine mounts `~/.gvx/docker` read-only at `/etc/docker`; dockerd reads
  `/etc/docker/daemon.json` as it does anywhere. `recipes.Prepare` writes the file before each
  start: the user's file, with `host.docker.internal:5001` (the registry recipe) added to
  `insecure-registries` when missing. The recipe's `--insecure-registry` flag moved into the file
  (dockerd refuses a setting given both ways); a command line that still names an insecure
  registry keeps the file out of `insecure-registries`.
- `recipes.Upgrade` adds the mount to an engine saved earlier and drops the recipe's own flag;
  it takes effect at the next start, as every upgrade does.
- Settings → Docker → Engine configuration edits the file (written whole: temp file + rename).
  On save, for a running engine with the mount: no change → nothing; a change to a key dockerd
  applies only at start (anything outside `debug, labels, live-restore, max-concurrent-*,
  max-download-attempts, default-runtime, runtimes, authorization-plugins, insecure-registries,
  registry-mirrors, shutdown-timeout, features`) → a toast offering Restart; otherwise
  `recipes.ReloadDockerd`: `rungv kill -s SIGHUP` and the engine's log is read for dockerd's verdict:
  `msg="Reloaded configuration"`, or `level=error msg="Error reloading configuration"
  error="…"`, whose `error` is shown. A paused engine, or a run without the mount or the control
  socket, gets a toast instead.
- The rename on the host is seen by the guest through the directory bind (verified).

### 12.3 Ports as rungv knows them

- `State.Ports` entries carry `Source` (static, auto, dynamic). Besides `rungv: publishing`,
  the supervisor reads `-auto-publish`'s `gvx: auto-published H -> G/P` and `gvx: unpublished …`
  lines, and keeps `State.Busy`: the host addresses `gvx: warning: could not auto-publish` named,
  until that port is published.
- Runs with a control socket then read `rungv port ls -f json` (debounced 250 ms, one at a time
  per run) after each such line and at ready, and take its list, which also has ports added
  with `port add`. Auto-published ports raise no control event (only `port add/rm` do), so the
  lines stay the trigger.
- Auto-publish skips guest ports that have a static rule (`TestDynamicLoopbackSkipsStaticRules`
  upstream), so dockerd's 2375, pinned to the app-private socket, never reaches TCP.
- UI: the sandbox's Connect tab marks auto-published ports and lists busy ones; a container's
  Connect tab says whether its published port reached this computer, is taken by something
  here (Open disabled), or is not published yet.

