<p align="center">
  <img src="assets/gvx-desktop-icon.svg" width="128" height="128" alt="A Go gopher in a space helmet">
</p>

<h1 align="center">GVX Desktop</h1>

<p align="center"><b>English</b> · <a href="README.ko.md">한국어</a></p>

GVX Desktop runs Linux containers as unprivileged [gvx](https://github.com/gfire-sigs/gvx) sandboxes without a separate Linux VM, a background daemon, or root privileges. Use it to manage sandboxes, images, volumes, snapshots, Docker containers, and Kubernetes clusters from a native desktop interface built with [ggui](https://github.com/ironpark/ggui).

<p align="center">
  <img src="assets/tour.gif" width="960" alt="Demo of the dashboard, sandbox editor and details, image search, Docker Engine and Kubernetes recipes, settings, and command palette">
</p>

## Install

### macOS

On an Apple silicon Mac running macOS 13 or later, install with [Homebrew](https://brew.sh):

```sh
brew install --cask ironpark/tap/gvx-desktop
```

To update, run `brew update` first. Homebrew refreshes its copy of a third-party tap only once every `HOMEBREW_AUTO_UPDATE_SECS` (24 hours by default), so `brew upgrade` alone may not see a new release yet. The cask quits the running app before it replaces it, and Homebrew opens the new version afterwards. An upgrade from 0.9.0 or earlier leaves the old version running, because Homebrew follows the cask that was installed: quit it from the tray menu and open the app again.

```sh
brew update
brew upgrade --cask ironpark/tap/gvx-desktop
```

For a manual install, download `gvx-desktop-<version>-macos-arm64.dmg` and its `.sha256` file from the [release page](https://github.com/ironpark/gvx-desktop/releases). Verify the checksum from the folder containing both files, then open the DMG and drag **GVX Desktop** to **Applications**.

```sh
shasum -a 256 -c gvx-desktop-*-macos-arm64.dmg.sha256
```

If macOS blocks an ad hoc signed build downloaded directly, try opening the app, then look for **Open Anyway** under **System Settings → Privacy & Security**. On macOS 14 and earlier, you can also Control-click the app and choose **Open**. After verifying the download, you can remove the quarantine attribute from the copy in **Applications** if needed:

```sh
xattr -dr com.apple.quarantine "/Applications/GVX Desktop.app"
```

The Homebrew cask handles the quarantine attribute for ad hoc signed builds automatically.

The macOS app bundle includes the official Docker CLI (`docker`, `buildx`, and `compose`) inside `Contents/Helpers`. You can expose the bundled Docker CLI and `rungvx` to your terminals via `~/.gvx/bin` under **Settings → Docker** and **Settings → rungvx**.

### Windows

Supports `amd64` (Intel/AMD) and `arm64` (Arm). **Windows Hypervisor Platform** must be enabled to run sandboxes.

#### Setup Installer (recommended)

Download `gvx-desktop-<version>-windows-amd64-setup.exe` (or `arm64`) from the [release page](https://github.com/ironpark/gvx-desktop/releases) and run the wizard. It installs per user without administrator privileges, creates Start Menu and Desktop shortcuts, optionally registers the bundled Docker CLI (`%USERPROFILE%\.gvx\bin`) on your user `PATH`, and supports clean in-place upgrades and uninstallation from Windows **Installed apps**.

#### PowerShell script

Install or update from PowerShell with the interactive [installer](install.ps1):

```powershell
irm https://raw.githubusercontent.com/ironpark/gvx-desktop/main/install.ps1 | iex
```

The script presents an interactive TUI in English, Korean, or Japanese (detected from your settings or Windows locale, or toggled with `L`), checks for running instances, verifies the SHA-256 checksum, and runs the Setup EXE when available (falling back to the portable ZIP for older releases).

- **Update** by running the same command again.
- **Install a specific version**: `$env:GVX_DESKTOP_VERSION = '0.12.0'`
- **Choose installer language**: `$env:GVX_DESKTOP_LANG = 'en'` (`en`, `ko`, or `ja`)
- **Non-interactive install**: `$env:GVX_DESKTOP_NONINTERACTIVE = '1'`
- **Control Desktop shortcut**: `$env:GVX_DESKTOP_DESKTOP_SHORTCUT = '0'` (or `'1'`)
- **Auto-launch or skip launch prompt**: `$env:GVX_DESKTOP_LAUNCH = '1'` (or `'0'`)

#### Portable ZIP

Download `gvx-desktop-<version>-windows-amd64.zip` (or `arm64`) from the [release page](https://github.com/ironpark/gvx-desktop/releases), extract it, and run `GVX Desktop.exe`. Each archive includes `GVX Desktop.exe`, `ggfx_driver.dll`, and the bundled Docker CLI (`bin/docker.exe`, `cli-plugins/docker-buildx.exe`, `cli-plugins/docker-compose.exe`).

### Linux

Download the package for your distribution and CPU (`amd64`/`x86_64` or `arm64`/`aarch64`) from the [release page](https://github.com/ironpark/gvx-desktop/releases):

```sh
sudo apt install ./gvx-desktop_*_amd64.deb      # Debian, Ubuntu
sudo dnf install ./gvx-desktop-*.x86_64.rpm     # Fedora, RHEL
```

The packages add **GVX Desktop** to the application menu, install the `gvx-desktop` command, and bundle the Docker CLI (`docker`, `buildx`, `compose`) under `/usr/lib/gvx-desktop/`. On other distributions, extract `gvx-desktop-<version>-linux-<arch>.tar.gz` over `/` or into `~/.local` (it contains the same `usr/` tree). Linux has no tray icon, so closing the window quits the app.

### Verify downloads

Every release ships `SHA256SUMS`, per-file `.sha256` files, `BUILD_INFO.txt`, and a signed SLSA v1.0 build provenance bundle (`gvx-desktop-v<version>.sigstore.json`). Verify any downloaded artifact with the GitHub CLI:

```sh
V=0.12.0  # release version
gh attestation verify "gvx-desktop-$V-macos-arm64.dmg" \
  --repo ironpark/gvx-desktop \
  --signer-workflow ironpark/gvx-desktop/.github/workflows/release.yml \
  --source-ref "refs/tags/v$V" \
  --deny-self-hosted-runners
```

## Run your first sandbox

1. Open GVX Desktop. If the dashboard prompts you to set up the engine, select **Install rungvx**. You can also point the app to an existing binary under **Settings → rungvx → rungvx path**.
2. On the dashboard, select **New sandbox**. Set **Image** to `nginx:alpine` and **Name** to `web`.
3. To reach nginx from the host, enter `127.0.0.1:8080:80` under **Published ports**.
4. Select **Save and start**. Open the sandbox details to view live **Logs**. If you published the port, open `http://127.0.0.1:8080` in your browser.

## Features

| Area | What you can do |
| --- | --- |
| **Dashboard** | Check engine status, view resource counts and per-sandbox usage, and start **Docker** and **Kubernetes** recipes. |
| **Sandboxes** | Create and edit sandboxes with a live `rungvx run` preview. Monitor CPU, memory, and BLKIO; filter live logs; add or remove published ports and host bind mounts on running sandboxes; configure NAT QoS and firewall policies; pause/resume, duplicate, and auto-start with the app. |
| **Images & Local Registry** | Unified **Cached Images** tab in default Sandbox Engine mode sharing `rungvx`'s EROFS layer cache between GVX sandboxes and Docker (split into **GVX Sandboxes** and **Docker** tabs in Compatibility mode). Search Docker Hub and Quay, browse image files and layers, pull/inspect/prune images, and use the **built-in local OCI registry** on `127.0.0.1:5001` (starts automatically with Docker or Kubernetes — no `registry:2` container required). |
| **Volumes & Snapshots** | Create, inspect, and prune `ext4lite` or `hostfs` volumes. Enable checkpointing on a sandbox to take millisecond gVisor snapshots (while keeping it running or stopping it) and restore or delete snapshots at any time. |
| **Docker** | Runs in native **Sandbox Engine** mode by default: each container runs directly as its own unprivileged gvx sandbox with the bundled `docker`, `buildx`, and `compose` CLIs, Compose `service_healthy` & `Healthcheck`, overlay networking, host bind mounts, BuildKit builder, opt-in LAN port publishing (`0.0.0.0`), and host SSH agent forwarding. Unmodified `docker:dind` remains available as **Compatibility** mode under **Settings → Docker**. |
| **Kubernetes (k3s)** | Launch single- or multi-node k3s clusters (with agent sandboxes), configure the `gvx-k3s` kubeconfig context, **Load into cluster** to import images directly, pull from the built-in local registry, manage Pods, Workloads, and Services, forward ports without `kubectl`, and enable ServiceLB. |
| **Labs (Experimental)** | **Local HTTPS routing (`*.localhost`)**: built-in loopback HTTP/HTTPS reverse proxy (`:18080` / `:18443`) with a local dev CA and dynamic TLS certificates for running containers, Compose services, sandboxes, and Kubernetes port forwards — no `/etc/hosts` edits. **AI agent MCP server**: built-in Streamable HTTP MCP server (`http://127.0.0.1:18787/mcp`) exposing 14 tools (sandboxes, exec, logs, checkpoint snapshots & restore, containers, images, local routes) with one-click Claude Code and JSON config copy. **Cross-architecture & per-container micro-VMs** (Apple silicon macOS): `linux/amd64` builds and container runs via Rosetta, opt-in native `linux/arm64` micro-VMs (`--runtime=microvm` / Compose `runtime: microvm`) for workloads needing a full Linux kernel, shared zero-copy EROFS image cache, and mixed `gvx ↔ microvm` Compose overlay networking. |
| **Desktop & Updates** | Native Zig-backed GPU renderer (`ggui v0.9.3`), tray mode on macOS and Windows, 7 languages (`en`, `ko`, `ja`, `zh-Hans`, `zh-Hant`, `de`, `hi`), Command Palette (`⌘K`) and page shortcuts (`⌘1`–`⌘9`), automatic `rungvx` and app updates (Sparkle 2 appcast with GitHub Releases fallback), and one-click `~/.gvx/bin` terminal PATH setup for `rungvx` and the bundled Docker CLI. |

The app follows your system language by default. Change it anytime under **Settings → General → Language**.

## Manage the rungvx engine

GVX Desktop uses the first `rungvx` (or legacy `rungv`) executable it finds in this order:

1. The path set under **Settings → rungvx → rungvx path**
2. The `GVX_RUNGVX` or `GVX_RUNGV` environment variable
3. `PATH`
4. `~/.gvx/bin`
5. Locations relative to the app (including bundle resources), then `./bin` in the working directory
6. `/opt/homebrew/bin` and `/usr/local/bin` on non-Windows systems

In each place it looks for `rungv` and `rungvx` (the name gvx gives it since v0.23). For compatibility with existing setups, installs and updates place the binary at `~/.gvx/bin/rungv` and also create a `rungvx` link beside it when the binary is `rungvx`.

To use `rungvx` from your terminal as well, select **Add to PATH** under **Settings → rungvx → Terminal PATH** to link it into `~/.gvx/bin` and add that directory to your shell profile (or user `PATH` on Windows). When the installed binary is `rungvx` (gvx v0.23 and later), `~/.gvx/bin` also gets a `rungvx` link to it; the app adds it after an install or update as well.

On macOS, `rungvx` requires the `com.apple.security.hypervisor` entitlement. GVX Desktop signs binaries it installs and offers **Sign rungvx** whenever an existing binary is missing the entitlement.

### Updates

Choose a channel under **Settings → rungvx updates → Channel**:

| Channel | How it installs `rungvx` |
| --- | --- |
| **Releases** | Downloads the latest [gvx release](https://github.com/gfire-sigs/gvx/releases) for your platform and verifies it against `SHA256SUMS`. |
| **From source** | Clones and builds `rungvx` locally from a Git repository and branch (defaults to gvx `main`). Requires Git and Go; the first build takes several minutes to compile gVisor. |

Startup update checks can be toggled under **Settings → rungvx updates** and **Settings → GVX Desktop updates**. Running sandboxes continue using the `rungvx` binary they started with.

## Development & Release

For building from source, running tests, preview screenshots, localization, package architecture, and reproducible releases, see **[DEVELOPMENT.md](DEVELOPMENT.md)**.

For version history and release notes, see **[CHANGELOG.md](CHANGELOG.md)**.

## License

GVX Desktop is licensed under the [Apache License 2.0](LICENSE).
