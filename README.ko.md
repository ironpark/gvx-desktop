<p align="center">
  <img src="assets/gvx-desktop-icon.svg" width="128" height="128" alt="우주 헬멧을 쓴 Go 고퍼">
</p>

<h1 align="center">GVX Desktop</h1>

<p align="center"><a href="README.md">English</a> · <b>한국어</b></p>

GVX Desktop은 별도의 Linux VM, 백그라운드 데몬, root 권한 없이 Linux 컨테이너를 비특권 [gvx](https://github.com/gfire-sigs/gvx) 샌드박스로 실행합니다. [ggui](https://github.com/ironpark/ggui)로 만든 네이티브 데스크톱 인터페이스에서 샌드박스, 이미지, 볼륨, 스냅샷, Docker 컨테이너, Kubernetes 클러스터를 관리할 수 있습니다.

<p align="center">
  <img src="assets/tour.gif" width="960" alt="대시보드, 샌드박스 편집 및 상세 화면, 이미지 검색, Docker Engine과 Kubernetes 레시피, 설정, 명령 팔레트 시연">
</p>

## 설치

### macOS

macOS 13 이상이 설치된 Apple Silicon Mac에서 [Homebrew](https://brew.sh)로 설치합니다.

```sh
brew install --cask ironpark/tap/gvx-desktop
```

업데이트할 때는 먼저 `brew update`를 실행합니다. Homebrew는 서드파티 tap을 `HOMEBREW_AUTO_UPDATE_SECS`(기본 24시간)마다 한 번만 새로 받으므로, `brew upgrade`만으로는 새 릴리스가 아직 보이지 않을 수 있습니다. cask는 실행 중인 앱을 종료한 뒤 교체하고, Homebrew가 새 버전을 다시 엽니다. 0.9.0 이하에서 업그레이드하면 Homebrew가 이미 설치된 cask를 따르므로 예전 버전이 계속 실행됩니다. 이때는 트레이 메뉴에서 종료한 뒤 앱을 다시 엽니다.

```sh
brew update
brew upgrade --cask ironpark/tap/gvx-desktop
```

직접 설치하려면 [릴리스 페이지](https://github.com/ironpark/gvx-desktop/releases)에서 `gvx-desktop-<version>-macos-arm64.dmg`와 함께 게시된 `.sha256` 파일을 다운로드합니다. 두 파일이 있는 폴더에서 체크섬을 확인한 다음 DMG를 열어 **GVX Desktop**을 **응용 프로그램** 폴더로 옮깁니다.

```sh
shasum -a 256 -c gvx-desktop-*-macos-arm64.dmg.sha256
```

직접 내려받은 ad hoc 서명 빌드를 macOS가 차단하면 앱을 한 번 열어 본 뒤 **시스템 설정 → 개인정보 보호 및 보안 → 그래도 열기**를 확인합니다. macOS 14 이하에서는 앱을 Control 키를 누른 채 클릭하고 **열기**를 선택할 수도 있습니다. 다운로드를 검증한 뒤 필요한 경우 **응용 프로그램** 폴더에 있는 앱의 격리 속성을 제거합니다.

```sh
xattr -dr com.apple.quarantine "/Applications/GVX Desktop.app"
```

Homebrew cask는 ad hoc 서명 빌드의 격리 속성을 자동으로 처리합니다.

macOS 앱 번들에는 공식 Docker CLI(`docker`, `buildx`, `compose`)가 `Contents/Helpers` 안에 포함되어 있습니다. **설정 → Docker** 및 **설정 → rungvx**에서 `~/.gvx/bin`을 통해 번들 Docker CLI와 `rungvx`를 터미널 `PATH`에 등록할 수 있습니다.

### Windows

`amd64`(Intel/AMD)와 `arm64`(Arm)를 지원합니다. 샌드박스를 실행하려면 **Windows 하이퍼바이저 플랫폼** 기능을 켜야 합니다.

#### 설치 프로그램 (권장)

[릴리스 페이지](https://github.com/ironpark/gvx-desktop/releases)에서 `gvx-desktop-<version>-windows-amd64-setup.exe`(또는 `arm64`)를 다운로드해 실행합니다. 관리자 권한 없이 현재 사용자 영역에 설치되며, 시작 메뉴와 바탕 화면 바로가기 생성, 번들 Docker CLI(`%USERPROFILE%\.gvx\bin`)의 사용자 `PATH` 등록 옵션, 깔끔한 업그레이드와 Windows **설치된 앱**에서의 제거를 지원합니다.

#### PowerShell 스크립트

인터랙티브 [설치 스크립트](install.ps1)로 PowerShell에서 바로 설치하거나 업데이트할 수 있습니다.

```powershell
irm https://raw.githubusercontent.com/ironpark/gvx-desktop/main/install.ps1 | iex
```

스크립트는 영어·한국어·일본어 TUI(설정 또는 Windows 로캘에서 자동 감지, 실행 중 `L` 키로 전환)를 제공하며, 실행 중인 앱 확인, SHA-256 체크섬 검증 후 릴리스에 설치 프로그램(`.exe`)이 있으면 이를 실행하고 이전 릴리스에서는 포터블 ZIP으로 자동 폴백합니다.

- **업데이트**: 같은 명령을 다시 실행합니다.
- **특정 버전 설치**: `$env:GVX_DESKTOP_VERSION = '0.11.4'`
- **설치 언어 지정**: `$env:GVX_DESKTOP_LANG = 'ko'` (`en`, `ko`, `ja`)
- **비대화형 설치**: `$env:GVX_DESKTOP_NONINTERACTIVE = '1'`
- **바탕 화면 바로가기 제어**: `$env:GVX_DESKTOP_DESKTOP_SHORTCUT = '0'` (또는 `'1'`)
- **설치 후 자동 실행/건너뛰기**: `$env:GVX_DESKTOP_LAUNCH = '1'` (또는 `'0'`)

#### 포터블 ZIP

[릴리스 페이지](https://github.com/ironpark/gvx-desktop/releases)에서 `gvx-desktop-<version>-windows-amd64.zip`(또는 `arm64`)을 내려받아 압축을 푼 뒤 `GVX Desktop.exe`를 실행합니다. 아카이브에는 `GVX Desktop.exe`, `ggfx_driver.dll`, 그리고 번들 Docker CLI(`bin/docker.exe`, `cli-plugins/docker-buildx.exe`, `cli-plugins/docker-compose.exe`)가 들어 있습니다.

### Linux

[릴리스 페이지](https://github.com/ironpark/gvx-desktop/releases)에서 배포판과 CPU(`amd64`/`x86_64` 또는 `arm64`/`aarch64`)에 맞는 패키지를 다운로드합니다.

```sh
sudo apt install ./gvx-desktop_*_amd64.deb      # Debian, Ubuntu
sudo dnf install ./gvx-desktop-*.x86_64.rpm     # Fedora, RHEL
```

패키지는 응용 프로그램 메뉴에 **GVX Desktop**을 추가하고 `gvx-desktop` 명령과 번들 Docker CLI(`docker`, `buildx`, `compose`)를 `/usr/lib/gvx-desktop/` 아래에 설치합니다. 다른 배포판에서는 `gvx-desktop-<version>-linux-<arch>.tar.gz`를 `/` 또는 `~/.local`에 푸세요(동일한 `usr/` 구조를 포함합니다). Linux에는 트레이 아이콘이 없어 창을 닫으면 앱이 종료됩니다.

### 다운로드 검증

모든 릴리스는 `SHA256SUMS`, 파일별 `.sha256`, `BUILD_INFO.txt`, 그리고 서명된 SLSA v1.0 빌드 프로비넌스 번들(`gvx-desktop-v<version>.sigstore.json`)을 함께 게시합니다. GitHub CLI로 다운로드한 파일을 검증할 수 있습니다.

```sh
V=0.11.4  # 릴리스 버전
gh attestation verify "gvx-desktop-$V-macos-arm64.dmg" \
  --repo ironpark/gvx-desktop \
  --signer-workflow ironpark/gvx-desktop/.github/workflows/release.yml \
  --source-ref "refs/tags/v$V" \
  --deny-self-hosted-runners
```

## 첫 샌드박스 실행

1. GVX Desktop을 엽니다. 대시보드에 엔진 설치 안내가 표시되면 **rungvx 설치**를 선택합니다. 이미 설치된 바이너리는 **설정 → rungvx → rungvx 경로**에서 지정할 수 있습니다.
2. 대시보드에서 **새 샌드박스**를 선택합니다. **이미지**에 `nginx:alpine`, **이름**에 `web`을 입력합니다.
3. 호스트에서 nginx에 접속하려면 **게시 포트**에 `127.0.0.1:8080:80`을 입력합니다.
4. **저장하고 시작**을 선택합니다. 샌드박스 상세 화면에서 실시간 **로그**를 확인할 수 있습니다. 포트를 게시했다면 브라우저에서 `http://127.0.0.1:8080`을 엽니다.

## 기능

| 영역 | 할 수 있는 일 |
| --- | --- |
| **대시보드** | 엔진 상태, 리소스 개수, 실행 중인 샌드박스 사용량을 확인하고 **Docker** 및 **Kubernetes** 레시피를 시작합니다. |
| **샌드박스** | 생성·편집 시 `rungvx run` 명령을 실시간으로 미리 봅니다. CPU·메모리·BLKIO 모니터링, 실시간 로그 필터링, 실행 중 동적 포트 및 호스트 바인드 마운트 변경, NAT QoS 및 방화벽 정책, 일시 정지/재개, 복제, 앱과 함께 자동 시작을 지원합니다. |
| **이미지 및 내장 로컬 레지스트리** | 기본 Sandbox Engine 모드에서는 GVX 샌드박스와 Docker가 `rungvx`의 EROFS 레이어 캐시를 함께 쓰는 통합 **캐시된 이미지** 탭을 제공합니다(호환성 모드에서는 **GVX 샌드박스** / **Docker** 탭으로 분리). Docker Hub·Quay 검색, 이미지 파일·레이어 탐색기, 그리고 Docker/Kubernetes와 함께 자동 시작되는 **내장 로컬 OCI 레지스트리**(`127.0.0.1:5001`, 별도 `registry:2` 컨테이너 불필요)를 제공합니다. |
| **볼륨 및 스냅샷** | `ext4lite` 또는 `hostfs` 볼륨을 생성·확인·정리합니다. 샌드박스 체크포인트를 켜면 실행 상태를 유지하거나 중지하면서 밀리초 단위의 gVisor 스냅샷을 저장하고 언제든 복원·삭제할 수 있습니다. |
| **Docker** | 기본 **Sandbox Engine** 모드에서 각 컨테이너를 독립된 비특권 gvx 샌드박스로 직접 실행합니다. 번들 `docker`·`buildx`·`compose` CLI, Compose `service_healthy` 및 `Healthcheck`, 오버레이 네트워킹, 호스트 바인드 마운트, BuildKit 빌더, 선택적 LAN 포트 게시(`0.0.0.0`), 호스트 SSH 에이전트 포워딩을 지원합니다. 원본 `docker:dind`는 **설정 → Docker**의 **호환성** 모드에서 계속 사용할 수 있습니다. |
| **Kubernetes (k3s)** | 단일 및 멀티 노드(에이전트 샌드박스) k3s 클러스터를 시작하고 `gvx-k3s` kubeconfig 컨텍스트를 구성합니다. **클러스터에 로드**로 이미지를 직접 가져오고, 내장 로컬 레지스트리와 연동하며, 파드·워크로드·서비스 관리, `kubectl` 없는 포트 포워딩, ServiceLB를 지원합니다. |
| **실험실 (Labs)** | **로컬 HTTPS 라우팅 (`*.localhost`)**: `/etc/hosts` 수정 없이 실행 중인 컨테이너, Compose 서비스, 샌드박스, Kubernetes 포트 포워드를 루프백 HTTP/HTTPS 리버스 프록시(`:18080` / `:18443`)와 로컬 개발 CA·동적 TLS 인증서로 연결합니다. **AI 에이전트 MCP 서버**: 14개 도구(샌드박스, exec, 로그, 체크포인트 스냅샷·복원, 컨테이너, 이미지, 로컬 라우트)를 제공하는 내장 Streamable HTTP MCP 서버(`http://127.0.0.1:18787/mcp`)와 Claude Code·JSON 설정 원클릭 복사를 지원합니다. **크로스 아키텍처 및 컨테이너별 마이크로 VM** (Apple Silicon macOS): Rosetta 기반 `linux/amd64` 이미지 빌드 및 컨테이너 실행, 전체 Linux 커널이 필요한 워크로드를 위한 `linux/arm64` 마이크로 VM 선택 실행(`--runtime=microvm` / Compose `runtime: microvm`), 제로카피 EROFS 이미지 캐시 공유, `gvx ↔ microvm` 혼합 Compose 오버레이 네트워킹을 지원합니다. |
| **데스크톱 및 업데이트** | Zig 기반 네이티브 GPU 렌더러(`ggui v0.9.3`), macOS·Windows 트레이 모드, 7개 언어(`en`, `ko`, `ja`, `zh-Hans`, `zh-Hant`, `de`, `hi`), 명령 팔레트(`⌘K`) 및 페이지 단축키(`⌘1`–`⌘9`), `rungvx` 및 앱 자동 업데이트(Sparkle 2 appcast + GitHub Releases 폴백), `rungvx`와 번들 Docker CLI의 `~/.gvx/bin` 터미널 PATH 원클릭 등록을 지원합니다. |

앱은 기본적으로 시스템 언어를 따릅니다. **설정 → 일반 → 언어**에서 언제든 바꿀 수 있습니다.

## rungvx 엔진 관리

GVX Desktop은 다음 순서로 검색해 처음 발견한 `rungvx`(또는 기존 `rungv`) 실행 파일을 사용합니다.

1. **설정 → rungvx → rungvx 경로**에 지정한 경로
2. `GVX_RUNGVX` 또는 `GVX_RUNGV` 환경 변수
3. `PATH`
4. `~/.gvx/bin`
5. 앱 실행 파일 기준 경로(번들 리소스 포함), 그다음 현재 작업 디렉터리의 `./bin`
6. Windows 이외 환경의 `/opt/homebrew/bin`과 `/usr/local/bin`

각 위치에서 `rungv`와 `rungvx`(gvx v0.23부터 쓰는 이름)를 찾습니다. 기존 환경과의 호환을 위해 설치와 업데이트는 `~/.gvx/bin/rungv`에 바이너리를 두고, 바이너리가 `rungvx`이면 옆에 `rungvx` 링크도 함께 만듭니다.

터미널에서도 `rungvx`를 바로 실행하려면 **설정 → rungvx → 터미널 PATH**에서 **PATH에 추가**를 선택해 `~/.gvx/bin`에 연결하고 셸 프로필(Windows는 사용자 `PATH`)에 등록하세요. 설치된 바이너리가 `rungvx`(gvx v0.23 이후)이면 `~/.gvx/bin`에 `rungvx` 링크도 함께 만듭니다. 설치나 업데이트 뒤에도 앱이 이 링크를 만듭니다.

macOS에서 `rungvx`에는 `com.apple.security.hypervisor` 권한이 필요합니다. 앱에서 설치한 바이너리는 자동으로 서명하며, 기존 바이너리에 권한이 없으면 **rungvx 서명**을 제공합니다.

### 업데이트

**설정 → rungvx 업데이트 → 채널**에서 설치 및 업데이트 채널을 선택합니다.

| 채널 | `rungvx` 설치 방식 |
| --- | --- |
| **릴리스** | 플랫폼에 맞는 최신 [gvx 릴리스](https://github.com/gfire-sigs/gvx/releases)를 내려받고 `SHA256SUMS`로 검증합니다. |
| **소스에서 빌드** | Git 저장소와 브랜치(기본값은 gvx `main`)에서 `rungvx`를 로컬 빌드합니다. Git과 Go가 필요하며, 첫 빌드에서는 gVisor를 컴파일하므로 몇 분 걸릴 수 있습니다. |

시작 시 자동 업데이트 확인은 **설정 → rungvx 업데이트**와 **설정 → GVX Desktop 업데이트**에서 켜거나 끌 수 있습니다. 실행 중인 샌드박스는 시작할 때 사용한 `rungvx` 바이너리로 계속 실행됩니다.

## 개발 및 릴리스

소스 빌드, 테스트, 화면 미리 보기, 번역, 패키지 아키텍처, 재현 가능한 릴리스 절차는 **[DEVELOPMENT.md](DEVELOPMENT.md)**를 참고하세요.

버전별 변경 이력과 릴리스 노트는 **[CHANGELOG.md](CHANGELOG.md)**에서 확인할 수 있습니다.

## 라이선스

GVX Desktop은 [Apache License 2.0](LICENSE)으로 배포됩니다.
