# Changelog

All notable changes to GVX Desktop are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.11.3] - 2026-11-26

### Added

- **Per-image micro-VM routing rules**: register specific Docker images or wildcard patterns (such as `postgres:16` or `ghcr.io/org/*`) to automatically run in the per-container Linux micro-VM runtime without needing `--runtime=microvm` (explicit `--runtime=gvx` still overrides). Manage rules live in **Labs → Cross-architecture & micro-VMs** or toggle them directly from an image's menu (**Always run in micro-VM** / **Stop running in micro-VM**) on the **Images** page, where matching images display a `micro-VM` badge across all 7 UI languages.
- **Git extension updates & manifest `[i18n]`**: added one-click **Update** for Git-installed extensions (`git pull --ff-only` and automatic rebuild, pausing for permission approval if new permissions are requested), and added `[i18n.<locale>]` localization support in `extension.toml` for extension names, descriptions, views, commands, and settings labels across the sidebar, extension headers, command palette, and settings.

### Changed

- **Stable left-aligned extension toolbar**: left-aligned lifted extension toolbars (`chrome = "toolbar"`) with compact toolbar button sizing so tabs stay anchored across view switches.
- The update notice for a Homebrew install gives `brew update` and `brew upgrade --cask ironpark/tap/gvx-desktop` as two commands, with the cask's full tap name, as the README does. The banner no longer puts the command in its sentence: **Copy command** copies both lines, and **Settings** shows them in a code block, as it does for the other package managers ([#18](https://github.com/ironpark/gvx-desktop/pull/18)).

## [0.11.2] - 2026-11-26

### Changed

- **Extension page header & layout polish**: replaced wide text host buttons (`Reload`, `Logs`, `Settings`) with compact tooltipped icon buttons separated by a vertical divider, and placed lifted extension toolbars (`chrome = "toolbar"`) inside a horizontally scrollable flex slot so host controls never squash or overlap at any window width.
- **Extension view spacing**: restored consistent outer padding (`12px / 16px`) on full-bleed `Fill` extension views, added `6px` pane gutters on both sides of `KindSplit` dividers, increased `KindCard` padding and `KindSelect` max width, and added `size = "small"` support for `KindTabs`.
- **Localized extension permissions**: translated all extension permission descriptions and risk badges (`High`, `Medium`, `Low`) across all 7 UI languages (`en`, `ko`, `ja`, `zh-Hans`, `zh-Hant`, `de`, `hi`).

## [0.11.1] - 2026-11-26

### Fixed

- Fixed installing Git extensions failing immediately with `git clone: context canceled` because `Model.initExtensions` passed the ephemeral `m.async` callback context to `ext.Manager.Start`, canceling the manager's lifetime context as soon as startup returned.
- Fixed uninstalling an extension while viewing its page to return to the dedicated **Extensions** page instead of **Settings**.

## [0.11.0] - 2026-11-26

### Added

- **Labs: Extensions system & Go SDK** (`internal/ext`, `internal/uiblock`, `internal/keychain`, `pkg/extsdk`):
  - Opt-in **Extensions** toggle in **Labs** that unlocks a dedicated **Extensions** sidebar page (between **Labs** and **Settings**) to install extensions from Git repositories or local directories, review and approve permissions, manage allowed host folders, inspect logs, and configure extension settings.
  - Sandboxed extension runtime inside isolated `rungvx` containers with scoped Docker API proxying, Host API (`/v1/sandboxes`, `/v1/snapshots`, `/v1/secrets`, `/v1/storage`, gesture-gated UI intents), OS keychain vault (`internal/keychain`), declarative native UI blocks (`internal/uiblock`), MCP tool bridging, and developer binary mode.
  - Public Go SDK (`github.com/ironpark/gvx-desktop/pkg/extsdk`) with the reference GitHub Actions extension ([`gvx-ext-github-actions`](https://github.com/ironpark/gvx-ext-github-actions)).
- **Volume directory** in the `rungvx` section of Settings places named volumes in a folder of your choice. The app passes it to `rungvx` as `GVX_VOLUME_DIR`, which gvx reads once [gfire-sigs/gvx#259](https://github.com/gfire-sigs/gvx/issues/259) is fixed. An empty setting keeps `rungvx`'s default. The setting applies only while **Layer cache directory** is empty, because `rungvx` keeps volumes in `<cacheDir>/volumes` when it gets `-cache-dir` ([#15](https://github.com/ironpark/gvx-desktop/issues/15), [#16](https://github.com/ironpark/gvx-desktop/pull/16), [#17](https://github.com/ironpark/gvx-desktop/pull/17)).
- The `rungvx` section of Settings warns when named volumes lie in `~/Library/Caches`, which macOS empties when the disk runs low, and when the volume directory does not apply because the `rungvx` in use ignores it or a layer cache directory is set. Each warning names the setting to change ([#16](https://github.com/ironpark/gvx-desktop/pull/16), [#17](https://github.com/ironpark/gvx-desktop/pull/17)).

### Changed

- **Switched user-facing `rungv` references to `rungvx`**: updated UI copy, menus, CLI previews, error messages, all 7 translation catalogs, and documentation to `rungvx` while keeping backwards compatibility with legacy `rungv` binaries, existing `settings.json` keys, and `$GVX_RUNGV` (with `$GVX_RUNGVX` checked first).
- **Windows color tray icon**: displays the full-color GVX mark in the Windows taskbar notification area (`Template: false`), while keeping the monochrome template silhouette in the macOS menu bar.

### Fixed

- Normalized trailing slashes in Sandbox Engine archive/stat path resolution (`internal/engine/archive.go`).
- The help text of **Layer cache directory** said that named volumes live in a `volumes` folder beside it. `rungvx` keeps them inside it, in `<cacheDir>/volumes`, and the text now also says that the volume directory then does not apply ([#16](https://github.com/ironpark/gvx-desktop/pull/16), [#17](https://github.com/ironpark/gvx-desktop/pull/17)).

## [0.10.2] - 2026-11-25

### Added

- **Settings → rungv updates** now lets you choose a specific published `rungv`/`rungvx` release from a **Release version** dropdown (or follow **Latest**), supporting downgrades, reinstalls, and download cancellation.

### Fixed

- **Install rungv** and **Update rungv** work again with gvx v0.23 and later, which renamed the program to `rungvx` and its release archives to `rungvx-<tag>-<os>-<arch>`. The app looked only for `rungv-*` archives and failed with "the release has no rungv for this system". It now takes either archive and installs the binary where it installed rungv before, so the path in Settings, the `~/.gvx/bin` link and the terminal's `PATH` keep working. It also finds a `rungvx` next to where it looks for `rungv`, reads `rungvx version`, recognizes `rungvx`'s own stderr lines (control error codes, published ports, the platform, and the lines kept out of container logs), and builds `./cmd/rungvx` when building from source. When the `rungv` in `~/.gvx/bin` is a `rungvx`, an install, an update or **Add to PATH** also puts a `rungvx` link beside it (a tracked copy on Windows), so terminals have the name gvx's own docs use ([#12](https://github.com/ironpark/gvx-desktop/issues/12)).
- Sandbox stderr logs now style `rungvx:` prefix lines in muted gray alongside `rungv:` lines.

## [0.10.1] - 2026-11-24

### Fixed

- In the Sandbox Engine, containers with a read-only root filesystem (`docker run --read-only`, Compose `read_only: true`) on a user network now resolve the other containers' names too. `netinit` could not replace their `/etc/resolv.conf` and only printed a warning, so an image's own file (Debian's names `1.1.1.1`) stayed and name lookups skipped the engine's DNS. It now binds a copy naming the relay over the file, as Docker binds its own `resolv.conf` ([#11](https://github.com/ironpark/gvx-desktop/pull/11)).

## [0.10.0] - 2026-11-24

### Added

- **Labs: Per-container Linux micro-VM runtime** (Apple silicon macOS, `--runtime=microvm` / Compose `runtime: microvm`):
  - Automatically runs `linux/amd64` containers inside a lightweight per-container Apple Virtualization.framework Linux micro-VM with Rosetta for Linux (`x86_64`), and optionally runs `linux/arm64` containers on a real Linux 6.18.64 kernel (no Rosetta required) for workloads that hit gVisor compatibility limits (`io_uring`, `iptables`/`nftables`, `FUSE`, `--privileged` VPNs, etc.).
  - Shares `rungv`'s EROFS image cache directly via zero-copy `virtio-blk` / `erofs` layer mounts, with persistent per-container `ext4` writable layers, `ext4lite` named volume sharing with host `flock` locking, directory and single-file bind mounts, `tmpfs`, cgroup v2 CPU/memory/PID limits, TCP port forwarding (`-p`/`-P`, Labs `*.localhost`, MCP), and full container controls (`exec`, TTY/Terminal, `top`, `stats`, `pause`/`unpause`, `cp`, `diff`, `export`, `commit`).
  - Adds per-VM resource controls in **Labs → Cross-architecture & micro-VMs** (`apple/container`-aligned defaults: 4 vCPUs clamped to host CPUs, 1024 MiB RAM), plus `micro-VM` and `amd64` badges in **Containers** and **Images** across all 7 languages.
- **Mixed Compose overlay networking (`gvx ↔ microvm`)**: micro-VM containers join user-defined Docker and Compose networks via vsock `gvx0` TUN and an in-guest `127.0.0.11:53` DNS relay bridged directly into `overlay.Fabric`, enabling bidirectional `gvx ↔ microvm` and `microvm ↔ microvm` service-name communication, live `docker network connect`/`disconnect`, and `--internal` network isolation without extra host listeners.

### Changed

- Upgraded `github.com/ironpark/gvx-desktop-vm` to `v0.2.0` (`vm-assets-v2` kernel & initramfs with `erofs`, `loop`, `tun`, and `wireguard` modules, container runtime RPCs, and vsock overlay transport).

### Fixed

- On macOS, quitting with ⌘Q, the Dock's **Quit**, or a quit Apple event now quits the app as the tray's **Quit** does, instead of only hiding it to the tray while **Keep running when the window closes** is on. A quit asked again while the sandboxes stop adds no second stop ([#7](https://github.com/ironpark/gvx-desktop/issues/7)).
- The Homebrew cask quits the running app before an upgrade or uninstall replaces it, so the old version no longer keeps running after `brew upgrade`. The app's update notice for a Homebrew install and the README now say `brew update && brew upgrade --cask gvx-desktop`, since `brew upgrade` alone may not see a release for up to a day ([#7](https://github.com/ironpark/gvx-desktop/issues/7)).
- Fixed Sandbox Engine containers whose image sets a non-root `USER` failing on user-defined and Compose networks with `netinit: setgroups: operation not permitted` (exit 126) by running the `netinit` wrapper as `0:0` before dropping to the container's configured user ([#9](https://github.com/ironpark/gvx-desktop/pull/9)).
- Fixed Sandbox Engine containers on a user-defined or Compose network failing to resolve other containers' names when their image ships its own `/etc/resolv.conf` (such as Debian-based `python:*-slim`, `redis`, and `postgres` images naming `1.1.1.1`) by having `netinit` point `/etc/resolv.conf` at `rungv`'s DNS relay (`10.0.2.3`), while leaving user-mounted `/etc/resolv.conf` or `/etc` untouched ([#10](https://github.com/ironpark/gvx-desktop/pull/10)).
- Fixed the Windows PowerShell installer (`install.ps1`) passing an unquoted `--dir` path containing spaces (`...\Programs\GVX Desktop`) to `gvx-desktop-*-setup.exe`, causing silent setup installs to exit with code 1.

## [0.9.0] - 2026-11-24

### Added

- **Built-in local OCI registry** (`internal/localreg` on `127.0.0.1:5001`): embedded OCI Distribution v1.1 / Registry V2 server with disk persistence, garbage collection, automatic start/stop alongside Docker and Kubernetes, Settings controls, and an **Images → Local Registry** tab — no `registry:2` container needed.
- **Labs: Local HTTPS routing (`*.localhost`)** (`internal/localproxy` on `:18080` / `:18443`): built-in loopback HTTP/HTTPS reverse proxy with a persistent local development CA, dynamic TLS leaf certificates, and automatic live routing for running containers, Compose services, sandboxes, and Kubernetes port forwards without editing `/etc/hosts`.
- **Labs: AI Agent MCP Server** (`internal/mcpserver` on `http://127.0.0.1:18787/mcp`, powered by `modelcontextprotocol/go-sdk`): embedded Streamable HTTP Model Context Protocol server exposing 14 tools (sandboxes, exec, logs, millisecond checkpoint snapshots & restore, containers, images, and local routes) with one-click endpoint, Claude Code CLI, and JSON config copy.
- **Bundled Docker CLI, Windows GUI Setup Installer, and Sparkle 2 updates**:
  - Ship pinned official `docker`, `docker-buildx`, and `docker-compose` binaries inside macOS, Windows, and Linux release packages (`release/docker-cli.env`).
  - Added self-contained Windows GUI setup and uninstaller wizard (`cmd/installer`, `gvx-desktop-<version>-windows-<arch>-setup.exe`) with optional bundled Docker CLI user `PATH` registration and clean upgrades.
  - Integrated Sparkle 2 `appcast.xml` app updates (Ed25519 signed) with GitHub Releases fallback.
- **One-click `rungv` terminal PATH management**: register or unregister `~/.gvx/bin` on your terminal `PATH` directly from **Settings → rungv**.
- **Expanded Docker Sandbox Engine capabilities**: container `Healthcheck` and Compose `service_healthy`, image history/import/search, `docker cp` on stopped containers, `--volumes-from`, container/image disk sizes, opt-in LAN port publishing (`0.0.0.0`), and host SSH agent forwarding (`--unix-forward`).
- **Multi-node Kubernetes & direct image loading**: add k3s agent sandboxes to clusters and import images directly via **Load into cluster** (`ctr -n k8s.io images import`).
- **`rungv` v0.21 feature adoption**: live port, mount, QoS, and NAT policy updates on running sandboxes, `rungv push`, `blkio` statistics, `bind_failed` event handling, and live mounts on snapshot restore.

### Changed

- **Native Sandbox Engine is now the default Docker mode**: each container runs directly as an unprivileged gvx sandbox; unmodified `docker:dind` remains available as **Compatibility** mode under **Settings → Docker**.
- **Unified Images view**: merged **GVX Sandboxes** and **Docker** image tabs into a single **Cached Images** tab in default Sandbox Engine mode (kept split in Compatibility mode).
- **Native Zig GPU renderer & UI refresh**: upgraded `ggui` to `v0.8.2` with the Zig-backed native GPU renderer (`ggfx`), added compact collapsible Docker and Kubernetes sidebar groups, and polished dialogs, terminal, and i18n across all 7 languages.
- **Cross-platform Labs**: enabled the **Labs** page on macOS, Windows, and Linux (Apple silicon Rosetta `amd64` cross-architecture builds remains macOS `arm64`-only inside Labs).
- **Documentation split**: moved contributor, architecture, and reproducible release documentation out of `README.md` into [DEVELOPMENT.md](DEVELOPMENT.md).

### Fixed

- Fixed Windows `/.gvx-netinit: permission denied` (#6) by mounting `netinit` through a read-only directory carrying hostfs `.gvx_meta` mode `0755`.
- Hardened Windows overlay UDP/DNS (`SIO_UDP_CONNRESET` handling and transient Winsock error recovery), BuildKit loopback TCP listener, legacy `POST /build` hostfs metadata/symlinks, and `GetAdaptersAddresses` guards.
- Fixed Sandbox Engine container rename `DNSNames`, restart bridge restoration, CPU quota enforcement, image list filters, quiet builds, running image removal (`rmi`), and line-streaming / `syncPortsDelay` data races.

## [0.8.0] - 2026-11-23

### Added

- Added the **Labs** page above Settings and hid engine menus when their engine is stopped.
- Added Sandbox Engine `diff`, `export`, and `commit` support, including for stopped containers.
- Added SLSA v1.0 build provenance attestations (`actions/attest`) and the reproducible release pipeline (`release/repro-build.sh`, `release/repro-pack.go`, `release/toolchain.env`).
- Added interactive TTY support for `docker run -t` and `docker exec -t` on Windows.

### Changed

- Upgraded `ggui` to `v0.7.1`, updated `gvx-desktop-vm` to `v0.1.1` with published `amd64` builder VM assets, and aligned per-OS `GOAMD64`/`GOARM64` levels with `gvx`.

### Fixed

- Fixed `rungv port ls` argument ordering relative to socket flags (#3).

## [0.7.0] - 2026-11-23

### Added

- Introduced the experimental native **Sandbox Engine** running Docker containers directly as gvx sandboxes without `dockerd`, including user-defined container overlay networking and a **Networks** page.
- Added Rosetta VM-based `linux/amd64` image builds on Apple silicon macOS.
- Adopted `rungv` v0.20 control-socket commands (`exec`, `cp`, `pause`/`unpause`, live updates).

### Changed

- Improved Docker Engine graceful shutdown, `daemon.json` reload, and published port tracking.
- Upgraded `ggui` to `v0.6.3`.

## [0.6.0] - 2026-11-21

### Added

- Added in-app self-updater checking GitHub Releases with SHA-256 verification.
- Added host-facing Docker API proxy (`internal/dockerproxy`), host folder sharing for Docker bind mounts (including Windows path conversion), cluster image registry (`127.0.0.1:5001`), and Kubernetes ServiceLB support.
- Surfaced port-publishing errors reported by `rungv`.

## [0.5.1] - 2026-11-20

### Added

- Added interactive multilingual PowerShell installer TUI (`install.ps1`) for Windows supporting English, Korean, and Japanese.

### Fixed

- Fixed intermittent terminal test flakiness.

## [0.5.0] - 2026-11-20

### Added

- Added image file/layer explorer for inspecting container images.
- Added customizable Git repository and branch selection for building `rungv` from source.

### Changed

- Polished update banner, size formatting, GVX naming, and translations; upgraded `ggui` to `v0.5.8`.

## [0.4.0] - 2026-11-19

### Added

- Added **Containers** page with Compose project grouping, resource stats, logs, and in-app container shell terminal.
- Added **Pods**, **Workloads**, and **Services** pages for k3s clusters with `kubectl`-free port forwarding and in-app pod terminal.

### Changed

- Unified CI release builds (including macOS `.app` and `.dmg`) on Linux runners and refreshed the recorded `assets/tour.gif` walkthrough.

## [0.3.0] - 2026-11-19

### Added

- Added automatic `docker` (`gvx` context) and `kubectl` (`gvx-k3s` context) setup and engine auto-resume on app startup.
- Added 5 additional UI languages: Japanese, Simplified Chinese, Traditional Chinese, German, and Hindi.

## [0.2.0] - 2026-11-19

### Added

- Added Windows (`amd64`, `arm64`) releases, one-line PowerShell installer (`install.ps1`), and Linux (`.deb`, `.rpm`, `.tar.gz`) releases.
- Added start-at-login and tray mode hiding from the macOS Dock.
- Added Apache License 2.0 and CI translation checks.

## [0.1.0] - 2026-11-18

### Added

- Initial release of GVX Desktop for Apple silicon macOS with Homebrew cask distribution.
- Sandbox management, image registry search (Docker Hub & Quay), `ext4lite`/`hostfs` volumes, checkpoint snapshots, `rungv` installer/updater, and English & Korean UI.

[Unreleased]: https://github.com/ironpark/gvx-desktop/compare/v0.11.3...HEAD
[0.11.3]: https://github.com/ironpark/gvx-desktop/compare/v0.11.2...v0.11.3
[0.11.2]: https://github.com/ironpark/gvx-desktop/compare/v0.11.1...v0.11.2
[0.11.1]: https://github.com/ironpark/gvx-desktop/compare/v0.11.0...v0.11.1
[0.11.0]: https://github.com/ironpark/gvx-desktop/compare/v0.10.2...v0.11.0
[0.10.2]: https://github.com/ironpark/gvx-desktop/compare/v0.10.1...v0.10.2
[0.10.1]: https://github.com/ironpark/gvx-desktop/compare/v0.10.0...v0.10.1
[0.10.0]: https://github.com/ironpark/gvx-desktop/compare/v0.9.0...v0.10.0
[0.9.0]: https://github.com/ironpark/gvx-desktop/compare/v0.8.0...v0.9.0
[0.8.0]: https://github.com/ironpark/gvx-desktop/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/ironpark/gvx-desktop/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/ironpark/gvx-desktop/compare/v0.5.1...v0.6.0
[0.5.1]: https://github.com/ironpark/gvx-desktop/compare/v0.5.0...v0.5.1
[0.5.0]: https://github.com/ironpark/gvx-desktop/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/ironpark/gvx-desktop/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/ironpark/gvx-desktop/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/ironpark/gvx-desktop/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/ironpark/gvx-desktop/releases/tag/v0.1.0
