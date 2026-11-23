<p align="center">
  <img src="assets/gvx-desktop-icon.svg" width="128" height="128" alt="A Go gopher in a space helmet">
</p>

<h1 align="center">GVX Desktop</h1>

<p align="center"><b>English</b> · <a href="README.ko.md">한국어</a></p>

GVX Desktop runs Linux containers as unprivileged [gvx](https://github.com/gfire-sigs/gvx) sandboxes without a separate Linux VM, a background daemon, or root privileges. Use it to manage sandboxes, images, volumes, and snapshots from a desktop interface built with [ggui](https://github.com/ironpark/ggui).

<p align="center">
  <img src="assets/tour.gif" width="960" alt="Demo of the dashboard, sandbox editor and details, image search, Docker Engine and Kubernetes recipes, settings, and command palette">
</p>

## Install

### macOS

On an Apple silicon Mac running macOS 13 or later, install with [Homebrew](https://brew.sh).

```sh
brew install --cask ironpark/tap/gvx-desktop
```

For a manual install, download the DMG and its `.sha256` file from the [release page](https://github.com/ironpark/gvx-desktop/releases). Check the download from the folder containing both files, then open the DMG and drag **GVX Desktop** to **Applications**.

```sh
shasum -a 256 -c gvx-desktop-*-macos-arm64.dmg.sha256
```

If macOS blocks an ad hoc signed build downloaded directly, try opening the app, then look for **Open Anyway** under **System Settings → Privacy & Security**. On macOS 14 and earlier, you can also try Control-clicking the app and choosing **Open**. After verifying the download, you can remove the quarantine attribute from the copy in **Applications** if needed.

```sh
xattr -dr com.apple.quarantine "/Applications/GVX Desktop.app"
```

The Homebrew cask handles the quarantine attribute for ad hoc signed builds.

### Windows

Install from PowerShell. Windows Hypervisor Platform must be enabled to run sandboxes.

```powershell
irm https://raw.githubusercontent.com/ironpark/gvx-desktop/main/install.ps1 | iex
```

The interactive [installer](install.ps1) checks for running instances, verifies the release ZIP against its published SHA-256 checksum, installs for the current user without administrator privileges, creates Start Menu and Desktop shortcuts, and offers to launch the app when finished. It defaults to English and supports English, Korean, and Japanese (detected from your settings or Windows locale, or toggled with `L`).

- **Update** by running the same command again.
- **Install a specific version** by setting `$env:GVX_DESKTOP_VERSION = '0.5.0'` before running the command.
- **Choose installer language** by setting `$env:GVX_DESKTOP_LANG = 'en'` (`en`, `ko`, or `ja`), or press `L` in the installer.
- **Non-interactive install** by setting `$env:GVX_DESKTOP_NONINTERACTIVE = '1'` (optional: `$env:GVX_DESKTOP_DESKTOP_SHORTCUT = '0'`, `$env:GVX_DESKTOP_LAUNCH = '1'`).
- **Uninstall** by deleting `%LOCALAPPDATA%\Programs\GVX Desktop` and the **GVX Desktop** Start Menu / Desktop shortcuts.

For a manual install, download the ZIP from the [release page](https://github.com/ironpark/gvx-desktop/releases). Choose `windows-amd64` for Intel or AMD, or `windows-arm64` for Arm. Extract the ZIP and open `GVX Desktop.exe`.

### Linux

Download the package for your distribution and CPU from the [release page](https://github.com/ironpark/gvx-desktop/releases): `amd64`/`x86_64` for Intel or AMD, `arm64`/`aarch64` for Arm.

```sh
sudo apt install ./gvx-desktop_*_amd64.deb      # Debian, Ubuntu
sudo dnf install ./gvx-desktop-*.x86_64.rpm     # Fedora, RHEL
```

The packages add **GVX Desktop** to the application menu and install the `gvx-desktop` command. On other distributions, extract the `linux-*.tar.gz` archive over `/` or into `~/.local`; it holds the same files under `usr/`. Each file has a `.sha256` beside it. Linux has no tray icon, so closing the window quits the app.

For other platforms, see [Build and run](#build-and-run).

## Run your first sandbox

1. Open GVX Desktop. If the dashboard prompts you to set up the engine, select **Install rungv**. You can also point the app to an existing binary under **Settings → Engine → rungv path**.
2. On the dashboard, select **New sandbox**. Set **Image** to `nginx:alpine` and **Name** to `web`.
3. To open nginx from the host, enter `127.0.0.1:8080:80` under **Published ports**.
4. Select **Save and start**. Open the sandbox details to view **Logs**. If you published the port, open `http://127.0.0.1:8080` in your browser.

## Features

| Area | What you can do |
| --- | --- |
| Dashboard | Check engine status, see resource counts and what each running sandbox uses, and launch recipes. |
| Sandboxes | Create and edit with a live `rungv run` preview. Start, stop, restart, duplicate, or delete sandboxes. See each running sandbox's CPU and memory, filter live logs, open or copy published ports, and start selected sandboxes with the app. |
| Images | Search Docker Hub or Quay, choose tags, and track pull progress. Inspect image metadata as JSON, run images in new sandboxes, or remove and prune images. |
| Volumes | Create `ext4lite` or `hostfs` volumes, see which sandboxes use them, open their location, and delete or prune them. |
| Snapshots | Enable checkpoint support for a sandbox to capture its running state. Choose whether to stop it or keep it running after capture. List, restore, and delete snapshots. |
| Containers | While the Docker Engine runs, list its containers with their state, image, ports, CPU and memory, grouped by Compose project. Filter them, start, stop, restart, or remove them one at a time or a whole project at once, read their logs, open a shell in a running one in the app's terminal, open their published ports, and copy `docker logs` and `docker exec` commands. |
| Pods | While the k3s cluster runs, list its pods with the status, readiness and restarts `kubectl get pods` shows, and the CPU and memory of `kubectl top pods` when metrics-server answers. Filter them, show or hide the system's pods, read each container's logs, open a shell in a running container in the app's terminal (with `kubectl`), see a pod's containers, node and owner, delete pods, and copy `kubectl logs`, `kubectl exec` and `kubectl describe` commands. Switch to Workloads to scale, restart or delete Deployments, StatefulSets, DaemonSets and Jobs and see their pods, or to Services to open the NodePorts the cluster publishes. Forward a port of a pod or a service to this computer without `kubectl`; forwards stop with their target, the cluster or the app. |
| Docker Engine | Start `docker:dind`, see whether `docker` uses its `gvx` context and set it up or switch to it, and manage its containers from the Docker menu and the tray as well. Folders of this computer are shared with the engine at their own paths (your home folder by default; edit the list in Settings → Docker), so `docker run -v` and Compose bind mounts of them work, and the Containers page flags a bind mount of a folder that is not shared. On Windows, `C:\Users\me` is at `/c/Users/me`; set `COMPOSE_CONVERT_WINDOWS_PATHS=1`. |
| Kubernetes | Start a single-node k3s cluster, detect its kubeconfig, see whether `kubectl` uses the cluster, and add it to `~/.kube/config` as the `gvx-k3s` context. Manage its pods and port forwards from the Kubernetes menu and the tray as well. A Docker Engine or cluster left running starts again with the app. Turn on the service load balancer (Settings → Kubernetes) to give LoadBalancer services an address: the node's ports 80 and 443 reach `127.0.0.1:8080` and `:8443`. |
| Image registry | Run `registry:2` on `127.0.0.1:5001` to bring images built in the Docker Engine to the cluster: push one from Images → Docker Engine → **Push to cluster registry** (or `docker push host.docker.internal:5001/app:dev`), and run it in a pod as `host.docker.internal:5001/app:dev`. Tools on this computer reach it as `localhost:5001`, which pods can pull too. |
| Desktop | Use native menus, start at login, and choose a light, dark, or system theme. On macOS and Windows, the app can keep running in the tray when you close its window. The app runs as a single instance. |

On macOS, use **⌘K** to open the command palette and **⌘1** through **⌘9** to switch pages in sidebar order. When the app hides in the menu bar, it also leaves the Dock.

The app follows the system language by default. Choose English, Korean, Japanese, Simplified or Traditional Chinese, German, or Hindi under **Settings → General → Language**.

## Manage the rungv engine

The app uses the first executable it finds in this order.

1. The path set under **Settings → Engine → rungv path**
2. The `GVX_RUNGV` environment variable
3. `PATH`
4. `~/.gvx/bin`
5. Locations relative to the app, including bundle resources, then `./bin` in the working directory
6. `/opt/homebrew/bin` and `/usr/local/bin` on non-Windows systems

On macOS, `rungv` needs the `com.apple.security.hypervisor` entitlement. The app signs binaries it installs and offers **Sign rungv** when it detects that an existing binary needs the entitlement.

### Updates

Choose a channel under **Settings → Updates → Channel**.

| Channel | How it installs rungv |
| --- | --- |
| **Releases** | Downloads the latest [gvx release](https://github.com/gfire-sigs/gvx/releases) for your platform and verifies the archive against `SHA256SUMS`. |
| **main, built here** | Fetches the latest commit on gvx's `main` branch and builds it locally. This requires Git and Go. The first build can take several minutes because it compiles gVisor. |

By default, the app checks the selected channel at startup and offers to install available updates. You can turn off startup checks under **Settings → Updates** or select **Check for updates** to check manually.

For source builds, the app keeps the gvx checkout in the `gvx-src` subdirectory of its data folder. It uses gvx's `scripts/repro-build.sh` when available on non-Windows systems and uses `go build` on Windows.

The app replaces the active binary if it is a regular file in a writable directory. Otherwise, it installs to `~/.gvx/bin` and sets **rungv path** if needed to use that copy. Running sandboxes continue with the binary they started with.

## Development

### Build and run

Use Go 1.27 or later. Run these commands from the repository root.

```sh
go build -o bin/gvx-desktop .
./bin/gvx-desktop
```

To develop against a local ggui checkout, create a Git-ignored `go.work` file. If you already have a workspace, run `go work use . ../ggui ../ggui/icons/lucide` to add the modules.

```sh
go work init . ../ggui ../ggui/icons/lucide
```

To build the macOS app bundle and DMG in `build/`, run the [ggui CLI](https://github.com/ironpark/ggui/blob/main/docs/cli.md).

```sh
ggui build --dmg
```

### Tests

```sh
go test -short ./...
```

With a working `rungv`, run the sandbox integration tests and the Docker Engine integration test.

```sh
go test ./internal/sandbox/
GVXD_DOCKER_TEST=1 go test ./internal/recipes/
```

### Preview and localization

Render screens to PNG with demo data and no running engine.

```sh
./bin/gvx-desktop -render-dir ./shots
./bin/gvx-desktop -render-dir ./shots-ko -render-lang ko
```

The README GIF is a walkthrough scripted in [tour.go](tour.go). To regenerate it, run [`ggui record`](https://github.com/ironpark/ggui/blob/main/docs/recording.md) from the repository root. It needs ggui CLI v0.6.0 or newer.

```sh
ggui record -o assets/tour.gif --width 1040 --frame mac --lossy 30 --fps 15
```

Each language has a catalog in [internal/ui](internal/ui) (`i18n_ko.go`, `i18n_ja.go`, `i18n_zh_hans.go`, `i18n_zh_hant.go`, `i18n_de.go`, `i18n_hi.go`) that uses the English text as keys. The localization test and `ggui i18n check` look for missing translations. Languages other than Korean were machine-translated and have not been reviewed by native speakers; corrections are welcome.

### Architecture

The gvx Go SDK restricts the process that calls it. To keep the GUI separate, the app starts one `rungv` child process per sandbox and reads management command output with `--format json`. When the app quits or receives SIGINT or SIGTERM, it stops the sandboxes it started.

| Package | Responsibility |
| --- | --- |
| [internal/rungv](internal/rungv) | Locate, install, sign, and call the `rungv` CLI. |
| [internal/sandbox](internal/sandbox) | Translate sandbox specs into CLI arguments and supervise processes, logs, ports, readiness, and snapshots. |
| [internal/registry](internal/registry) | Search Docker Hub and Quay and list image tags. |
| [internal/dockerapi](internal/dockerapi) | Call the Docker Engine API over the `docker:dind` Unix socket. |
| [internal/recipes](internal/recipes) | Define the Docker Engine, Kubernetes and image registry presets. |
| [internal/store](internal/store) | Store `sandboxes.json` and `settings.json` in the user configuration directory. |
| [internal/ui](internal/ui) | Implement the ggui screens. |

### Release

Update `version` in [ggui.toml](ggui.toml), then push the matching `vX.Y.Z` tag. The [release workflow](.github/workflows/release.yml) builds with the pinned Go toolchain and `ggui` CLI in [release/toolchain.env](release/toolchain.env) via [release/repro-build.sh](release/repro-build.sh), verifies bit-for-bit reproducibility in CI, signs SLSA v1.0 build provenance with Sigstore (`actions/attest`), and publishes the Apple silicon DMG, Windows amd64 and arm64 ZIPs, Linux amd64 and arm64 tarballs and Debian and RPM packages, checksums (`SHA256SUMS` and per-file `.sha256`), `BUILD_INFO.txt`, and the offline `.sigstore.json` provenance bundle. For stable tags, it also updates the [Homebrew cask](https://github.com/ironpark/homebrew-tap) with [release/cask.sh](release/cask.sh).

Verify a downloaded release file against its SLSA build provenance with the GitHub CLI:

```sh
gh attestation verify gvx-desktop-0.7.0-windows-amd64.zip \
  --repo ironpark/gvx-desktop \
  --signer-workflow ironpark/gvx-desktop/.github/workflows/release.yml \
  --deny-self-hosted-runners
```

To reproduce the release packages locally from a tagged checkout, or build a standalone binary directly from `proxy.golang.org`:

```sh
./release/repro-build.sh install-ggui
./release/repro-build.sh package windows amd64 0.7.0 ./out
./release/repro-build.sh package linux amd64 0.7.0 ./out

GO111MODULE=on GOPROXY=https://proxy.golang.org/ GOSUMDB=sum.golang.org \
  CGO_ENABLED=0 GOOS=windows GOARCH=amd64 GOAMD64=v1 GOTOOLCHAIN=go1.27.4 \
  go install -trimpath -pgo=off -compiler=gc \
  -ldflags='-buildid= -s -w -X main.version=0.7.0 -X github.com/ironpark/ggui.buildID=io.github.ironpark.gvx-desktop' \
  -- github.com/ironpark/gvx-desktop@v0.7.0
```

Set the repository variable `CHOCO_PUBLISH` to `true` to enable Chocolatey packaging with [release/choco.sh](release/choco.sh). The workflow header lists the relevant secrets. To regenerate the PNG assets for the app icon and sidebar mark, use [assets/svg2png.swift](assets/svg2png.swift).

## License

GVX Desktop is licensed under the [Apache License 2.0](LICENSE).
