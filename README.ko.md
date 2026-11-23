<p align="center">
  <img src="assets/gvx-desktop-icon.svg" width="128" height="128" alt="우주 헬멧을 쓴 Go 고퍼">
</p>

<h1 align="center">GVX Desktop</h1>

<p align="center"><a href="README.md">English</a> · <b>한국어</b></p>

GVX Desktop은 Linux 컨테이너를 별도의 Linux VM, 백그라운드 데몬, root 권한 없이 [gvx](https://github.com/gfire-sigs/gvx) 샌드박스에서 실행합니다. [ggui](https://github.com/ironpark/ggui)로 만든 데스크톱 인터페이스에서 샌드박스, 이미지, 볼륨, 스냅샷을 관리할 수 있습니다.

<p align="center">
  <img src="assets/tour.gif" width="960" alt="대시보드, 샌드박스 편집 및 상세 화면, 이미지 검색, Docker Engine과 Kubernetes 레시피, 설정, 명령 팔레트 시연">
</p>

## 설치

### macOS

macOS 13 이상이 설치된 Apple Silicon Mac에서 [Homebrew](https://brew.sh)로 설치합니다.

```sh
brew install --cask ironpark/tap/gvx-desktop
```

직접 설치하려면 [릴리스 페이지](https://github.com/ironpark/gvx-desktop/releases)에서 DMG와 함께 게시된 `.sha256` 파일을 다운로드합니다. 두 파일이 있는 폴더에서 체크섬을 확인한 다음, DMG를 열어 **GVX Desktop**을 **응용 프로그램** 폴더로 옮깁니다.

```sh
shasum -a 256 -c gvx-desktop-*-macos-arm64.dmg.sha256
```

직접 내려받은 ad hoc 서명 빌드를 macOS가 차단하면, 앱을 한 번 열어 본 뒤 **시스템 설정 → 개인정보 보호 및 보안 → 그래도 열기**를 확인합니다. macOS 14 이하에서는 앱을 Control 키를 누른 채 클릭하고 **열기**를 선택할 수도 있습니다. 다운로드를 검증한 뒤 필요한 경우 **응용 프로그램** 폴더에 있는 앱의 격리 속성을 제거합니다.

```sh
xattr -dr com.apple.quarantine "/Applications/GVX Desktop.app"
```

Homebrew cask는 ad hoc 서명 빌드의 격리 속성을 자동으로 제거합니다.

### Windows

PowerShell에서 설치합니다. 샌드박스를 실행하려면 **Windows 하이퍼바이저 플랫폼** 기능을 켜야 합니다.

```powershell
irm https://raw.githubusercontent.com/ironpark/gvx-desktop/main/install.ps1 | iex
```

인터랙티브 [설치 스크립트](install.ps1)는 실행 중인 GVX Desktop 확인 및 종료, SHA-256 체크섬 검증, 관리자 권한 없는 현재 사용자 설치, 시작 메뉴·바탕 화면 바로가기 생성, 설치 후 실행 여부 선택을 TUI로 제공합니다. 기본 언어는 영어이며 사용자 설정이나 Windows 로캘에 따라 영어·한국어·일본어를 지원합니다(설치 중 `L` 키로 전환 가능).

- **업데이트**하려면 같은 명령을 다시 실행합니다.
- **특정 버전**을 설치하려면 명령을 실행하기 전에 `$env:GVX_DESKTOP_VERSION = '0.5.0'`을 설정합니다.
- **설치 언어 지정**은 `$env:GVX_DESKTOP_LANG = 'ko'` (`en`, `ko`, `ja`)로 설정하거나 설치 화면에서 `L` 키를 누릅니다.
- **비대화형 설치**는 `$env:GVX_DESKTOP_NONINTERACTIVE = '1'`을 설정합니다(선택: `$env:GVX_DESKTOP_DESKTOP_SHORTCUT = '0'`, `$env:GVX_DESKTOP_LAUNCH = '1'`).
- **제거**하려면 `%LOCALAPPDATA%\Programs\GVX Desktop` 폴더와 시작 메뉴·바탕 화면의 **GVX Desktop** 바로 가기를 삭제합니다.

직접 설치하려면 [릴리스 페이지](https://github.com/ironpark/gvx-desktop/releases)에서 PC에 맞는 ZIP을 다운로드합니다. Intel과 AMD는 `windows-amd64`, Arm은 `windows-arm64`를 선택합니다. 압축을 푼 뒤 `GVX Desktop.exe`를 실행합니다.

### Linux

[릴리스 페이지](https://github.com/ironpark/gvx-desktop/releases)에서 배포판과 CPU에 맞는 패키지를 다운로드합니다. Intel과 AMD는 `amd64`/`x86_64`, Arm은 `arm64`/`aarch64`를 선택합니다.

```sh
sudo apt install ./gvx-desktop_*_amd64.deb      # Debian, Ubuntu
sudo dnf install ./gvx-desktop-*.x86_64.rpm     # Fedora, RHEL
```

패키지는 응용 프로그램 메뉴에 **GVX Desktop**을 추가하고 `gvx-desktop` 명령을 설치합니다. 다른 배포판에서는 `linux-*.tar.gz`를 `/` 또는 `~/.local`에 풉니다. 같은 파일이 `usr/` 아래에 들어 있습니다. 파일마다 `.sha256`이 함께 있습니다. Linux에는 트레이 아이콘이 없어서 창을 닫으면 앱이 종료됩니다.

다른 플랫폼에서는 [빌드와 실행](#빌드와-실행)을 참고하세요.

## 첫 샌드박스 실행

1. GVX Desktop을 엽니다. 대시보드에 엔진 설치 안내가 표시되면 **rungv 설치**를 선택합니다. 이미 설치된 바이너리는 **설정 → 엔진 → rungv 경로**에서 지정할 수 있습니다.
2. 대시보드에서 **새 샌드박스**를 선택합니다. **이미지**에 `nginx:alpine`을, **이름**에 `web`을 입력합니다.
3. 호스트에서 nginx에 접속하려면 **게시 포트**에 `127.0.0.1:8080:80`을 입력합니다.
4. **저장하고 시작**을 선택합니다. 샌드박스 상세 화면에서 **로그**를 확인할 수 있습니다. 포트를 게시했다면 브라우저에서 `http://127.0.0.1:8080`을 엽니다.

## 기능

| 영역 | 할 수 있는 일 |
| --- | --- |
| 대시보드 | 엔진 상태와 리소스 개수, 실행 중인 샌드박스의 사용량을 확인하고 레시피를 실행합니다. |
| 샌드박스 | 생성하거나 편집할 때 `rungv run` 명령을 미리 봅니다. 샌드박스를 시작, 중지, 재시작, 복제, 삭제할 수 있습니다. 실행 중인 샌드박스의 CPU와 메모리를 확인하고, 실시간 로그를 필터링하고 게시 포트를 열거나 복사하며, 선택한 샌드박스를 앱과 함께 시작할 수 있습니다. |
| 이미지 | Docker Hub와 Quay에서 이미지를 검색하고 태그를 선택합니다. 가져오기 진행 상황과 JSON 메타데이터를 확인하고, 이미지를 새 샌드박스에서 실행하거나 삭제 및 정리할 수 있습니다. |
| 볼륨 | `ext4lite` 또는 `hostfs` 볼륨을 만들고 어느 샌드박스가 사용하는지 확인합니다. 볼륨의 위치를 열거나 볼륨을 삭제 및 정리할 수 있습니다. |
| 스냅샷 | 샌드박스에서 체크포인트를 활성화하면 실행 상태를 저장할 수 있습니다. 저장 후 샌드박스를 계속 실행할지 선택하고, 스냅샷 목록을 보거나 복원 및 삭제할 수 있습니다. |
| 컨테이너 | Docker Engine이 실행 중일 때 컨테이너의 상태, 이미지, 포트, CPU와 메모리를 Compose 프로젝트별로 묶어 확인합니다. 컨테이너를 필터링하고 하나씩 또는 프로젝트 전체를 한 번에 시작, 중지, 재시작, 제거하며, 로그를 읽고 실행 중인 컨테이너의 셸을 앱 안의 터미널에서 열며, 게시 포트를 열거나 `docker logs`, `docker exec` 명령을 복사할 수 있습니다. |
| 파드 | k3s 클러스터가 실행 중일 때 `kubectl get pods`처럼 파드의 상태, 준비 상태, 재시작 횟수를 확인하고, metrics-server가 응답하면 `kubectl top pods`처럼 CPU와 메모리도 확인합니다. 파드를 필터링하고 시스템 파드를 보이거나 숨기며, 컨테이너별 로그를 읽고 실행 중인 컨테이너의 셸을 앱 안의 터미널에서 열며(`kubectl` 필요), 파드의 컨테이너, 노드, 소유자를 확인하고, 파드를 삭제하거나 `kubectl logs`, `kubectl exec`, `kubectl describe` 명령을 복사할 수 있습니다. 워크로드 보기에서는 Deployment, StatefulSet, DaemonSet, Job의 복제본 수를 바꾸거나 재시작, 삭제하고 그 파드를 확인하며, 서비스 보기에서는 클러스터가 게시한 NodePort를 엽니다. 파드나 서비스의 포트를 `kubectl` 없이 이 컴퓨터로 전달할 수 있으며, 전달은 대상이나 클러스터가 멈추거나 앱을 끄면 함께 멈춥니다. |
| Docker Engine | `docker:dind`를 시작하고, `docker`가 `gvx` 컨텍스트를 쓰는지 확인해 설정하거나 전환하며, 컨테이너를 Docker 메뉴와 트레이에서도 관리합니다. 이 컴퓨터의 폴더를 같은 경로로 엔진과 공유하므로(기본은 홈 폴더, 설정 → Docker에서 목록 편집) 그 폴더를 쓰는 `docker run -v`와 Compose 바인드 마운트가 동작하며, 공유되지 않은 폴더를 바인드 마운트한 컨테이너는 컨테이너 화면에 표시됩니다. Windows에서는 `C:\Users\me`가 `/c/Users/me`에 있으니 `COMPOSE_CONVERT_WINDOWS_PATHS=1`을 설정하세요. |
| Kubernetes | 단일 노드 k3s 클러스터를 시작하고 kubeconfig를 감지하며, `kubectl`이 클러스터를 쓰는지 확인해 `~/.kube/config`에 `gvx-k3s` 컨텍스트로 추가합니다. 파드와 포트 전달을 Kubernetes 메뉴와 트레이에서도 관리합니다. 실행 중이던 Docker Engine이나 클러스터는 앱을 다시 켜면 다시 시작됩니다. 서비스 로드 밸런서(설정 → Kubernetes)를 켜면 LoadBalancer 서비스가 주소를 받고, 노드의 포트 80과 443이 `127.0.0.1:8080`과 `:8443`에 닿습니다. |
| 이미지 레지스트리 | `127.0.0.1:5001`에서 `registry:2`를 실행해 Docker Engine에서 빌드한 이미지를 클러스터로 가져갑니다. 이미지 → Docker Engine → **클러스터 레지스트리로 푸시**(또는 `docker push host.docker.internal:5001/app:dev`)로 올리고, 파드에서 `host.docker.internal:5001/app:dev`로 실행합니다. 이 컴퓨터의 도구는 `localhost:5001`로 접속하며, 파드도 이 이름으로 가져올 수 있습니다. |
| 데스크톱 | 네이티브 메뉴, 로그인 시 자동 시작, 라이트·다크·시스템 화면 모드를 지원합니다. macOS와 Windows에서는 창을 닫아도 트레이에서 앱을 계속 실행할 수 있습니다. 앱은 한 번에 하나만 실행됩니다. |

macOS에서는 **⌘K**로 명령 팔레트를 열고 **⌘1**부터 **⌘9**까지의 단축키로 사이드바 순서대로 화면을 전환합니다. 앱이 메뉴 막대로 숨겨지면 Dock에서도 아이콘이 사라집니다.

앱은 기본적으로 시스템 언어를 따릅니다. **설정 → 일반 → 언어**에서 영어, 한국어, 일본어, 중국어(간체·번체), 독일어, 힌디어 중에서 선택할 수 있습니다.

## rungv 엔진 관리

앱은 다음 순서로 검색해 처음 발견한 실행 파일을 사용합니다.

1. **설정 → 엔진 → rungv 경로**에 지정한 경로
2. `GVX_RUNGV` 환경 변수
3. `PATH`
4. `~/.gvx/bin`
5. 앱 실행 파일 기준 경로(번들 리소스 포함), 그다음 현재 작업 디렉터리의 `./bin`
6. Windows 이외 환경의 `/opt/homebrew/bin`과 `/usr/local/bin`

macOS에서 `rungv`에는 `com.apple.security.hypervisor` 권한이 필요합니다. 앱에서 설치한 바이너리는 자동으로 서명합니다. 기존 바이너리에 권한이 없으면 **rungv 서명**을 선택할 수 있습니다.

### 업데이트

**설정 → 업데이트 → 채널**에서 설치 및 업데이트에 사용할 채널을 선택합니다.

| 채널 | rungv 설치 방식 |
| --- | --- |
| **릴리스** | 플랫폼에 맞는 최신 [gvx 릴리스](https://github.com/gfire-sigs/gvx/releases)를 내려받고 `SHA256SUMS`로 검증합니다. |
| **main (소스 빌드)** | gvx의 `main` 브랜치에서 최신 커밋을 가져와 로컬에서 빌드합니다. Git과 Go가 필요하며, 첫 빌드에서는 gVisor를 컴파일하므로 몇 분 걸릴 수 있습니다. |

앱은 기본적으로 시작할 때 선택한 채널을 확인하고 업데이트가 있으면 설치를 안내합니다. **설정 → 업데이트**에서 시작 시 확인을 끄거나 **업데이트 확인**을 눌러 직접 확인할 수 있습니다.

소스 빌드에 쓰는 gvx 저장소는 앱 데이터 폴더의 `gvx-src`에 저장합니다. Windows에서는 `go build`를 사용합니다. 그 외 환경에서는 gvx의 `scripts/repro-build.sh`가 있으면 해당 스크립트를 실행합니다.

현재 바이너리가 쓰기 가능한 디렉터리의 일반 파일이면 그 자리에 교체합니다. 그 외에는 `~/.gvx/bin`에 설치하고, 필요한 경우 **rungv 경로**를 그쪽으로 지정합니다. 실행 중인 샌드박스는 시작할 때 사용한 바이너리로 계속 실행됩니다.

## 개발

### 빌드와 실행

Go 1.27 이상이 필요합니다. 저장소 루트에서 다음 명령을 실행합니다.

```sh
go build -o bin/gvx-desktop .
./bin/gvx-desktop
```

로컬에 체크아웃한 ggui로 개발하려면 Git에서 추적하지 않는 `go.work`를 만듭니다. 워크스페이스가 이미 있으면 `go work use . ../ggui ../ggui/icons/lucide`로 모듈을 추가합니다.

```sh
go work init . ../ggui ../ggui/icons/lucide
```

macOS 앱 번들과 DMG를 `build/`에 만들려면 [ggui CLI](https://github.com/ironpark/ggui/blob/main/docs/cli.md)를 사용합니다.

```sh
ggui build --dmg
```

### 테스트

```sh
go test -short ./...
```

`rungv`가 작동하는 환경에서는 샌드박스와 Docker Engine 통합 테스트도 실행할 수 있습니다.

```sh
go test ./internal/sandbox/
GVXD_DOCKER_TEST=1 go test ./internal/recipes/
```

### 화면 미리 보기와 번역

엔진을 실행하지 않고 데모 데이터로 화면을 PNG로 렌더링합니다.

```sh
./bin/gvx-desktop -render-dir ./shots
./bin/gvx-desktop -render-dir ./shots-ko -render-lang ko
```

README의 GIF는 [tour.go](tour.go)에 작성된 앱 둘러보기입니다. 저장소 루트에서 [`ggui record`](https://github.com/ironpark/ggui/blob/main/docs/recording.md)를 실행하면 다시 만들 수 있습니다. ggui CLI v0.6.0 이상이 필요합니다.

```sh
ggui record -o assets/tour.gif --width 1040 --frame mac --lossy 30 --fps 15
```

언어마다 [internal/ui](internal/ui)에 카탈로그가 있고(`i18n_ko.go`, `i18n_ja.go`, `i18n_zh_hans.go`, `i18n_zh_hant.go`, `i18n_de.go`, `i18n_hi.go`), 코드에 있는 영어 문구를 키로 사용합니다. 번역 테스트와 `ggui i18n check`가 빠진 문구를 확인합니다. 한국어 외의 언어는 기계 번역이며 원어민 검수를 거치지 않았습니다. 수정 제안을 환영합니다.

### 동작 방식

gvx의 Go SDK는 자신을 호출한 프로세스의 동작을 제한합니다. GUI는 샌드박스마다 별도의 `rungv` 자식 프로세스를 실행하고, 관리 명령의 출력은 `--format json`으로 읽습니다. 앱을 종료하거나 앱이 SIGINT 또는 SIGTERM을 받으면, 앱이 시작한 샌드박스도 중지됩니다.

| 패키지 | 역할 |
| --- | --- |
| [internal/rungv](internal/rungv) | `rungv` CLI를 검색, 설치, 서명, 실행합니다. |
| [internal/sandbox](internal/sandbox) | 샌드박스 설정을 CLI 인자로 변환하고 프로세스, 로그, 포트, 준비 상태, 스냅샷을 관리합니다. |
| [internal/registry](internal/registry) | Docker Hub와 Quay에서 이미지를 검색하고 태그 목록을 가져옵니다. |
| [internal/dockerapi](internal/dockerapi) | `docker:dind`의 Unix 소켓으로 Docker Engine API를 호출합니다. |
| [internal/recipes](internal/recipes) | Docker Engine, Kubernetes, 이미지 레지스트리 프리셋을 정의합니다. |
| [internal/store](internal/store) | 사용자 설정 디렉터리에 `sandboxes.json`과 `settings.json`을 저장합니다. |
| [internal/ui](internal/ui) | ggui 화면을 구현합니다. |

### 릴리스

[ggui.toml](ggui.toml)의 `version`을 바꾸고 해당 버전의 `vX.Y.Z` 태그를 푸시합니다. [릴리스 워크플로](.github/workflows/release.yml)는 [release/toolchain.env](release/toolchain.env)에 고정된 Go 툴체인과 `ggui` CLI를 [release/repro-build.sh](release/repro-build.sh)로 실행해 빌드하고, CI에서 Windows ZIP, Linux tarball, Debian 패키지의 비트 단위 재현성을 확인한 뒤 Sigstore(`actions/attest`)로 SLSA v1.0 빌드 프로비넌스에 서명합니다. 이후 Apple Silicon용 DMG, Windows amd64·arm64용 ZIP, Linux amd64·arm64용 tarball과 Debian·RPM 패키지, 체크섬(`SHA256SUMS` 및 파일별 `.sha256`), `BUILD_INFO.txt`, 오프라인 검증용 `.sigstore.json` 번들을 한 번에 게시합니다. [release/cask.sh](release/cask.sh)로 [Homebrew cask](https://github.com/ironpark/homebrew-tap)도 갱신합니다.

다운로드한 릴리스 파일의 SLSA 빌드 프로비넌스는 GitHub CLI로 검증할 수 있습니다.

```sh
gh attestation verify gvx-desktop-0.7.0-windows-amd64.zip \
  --repo ironpark/gvx-desktop \
  --signer-workflow ironpark/gvx-desktop/.github/workflows/release.yml \
  --source-ref refs/tags/v0.7.0 \
  --deny-self-hosted-runners
```

태그 체크아웃에서 릴리스 패키지를 로컬로 재현하거나 `proxy.golang.org`에서 독립 실행 파일을 바로 설치하려면 아래 명령을 사용합니다(`v0.7.0`과 `0.7.0`을 대상 릴리스 버전으로 교체).

```sh
./release/repro-build.sh install-ggui
./release/repro-build.sh package windows amd64 0.7.0 ./out
./release/repro-build.sh package linux amd64 0.7.0 ./out

GO111MODULE=on GOPROXY=https://proxy.golang.org/ GOSUMDB=sum.golang.org \
  GOENV=off GOWORK=off GOTELEMETRY=off GOFIPS140=off GOEXPERIMENT= GODEBUG= GOFLAGS= GOBIN= \
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOAMD64=v1 GOTOOLCHAIN=go1.27.4 \
  go install -trimpath -pgo=off -compiler=gc \
  -ldflags='-s -w -buildid= -X main.version=0.7.0 -X github.com/ironpark/ggui.buildID=io.github.ironpark.gvx-desktop' \
  -- github.com/ironpark/gvx-desktop@v0.7.0
```

저장소 변수 `CHOCO_PUBLISH`를 `true`로 설정하면 [release/choco.sh](release/choco.sh)를 사용하는 Chocolatey 패키징이 활성화됩니다. 관련 시크릿은 워크플로 파일 상단에 설명되어 있습니다. 앱 아이콘과 사이드바 마크의 PNG가 필요하면 [assets/svg2png.swift](assets/svg2png.swift)로 다시 생성합니다.

## 라이선스

GVX Desktop은 [Apache License 2.0](LICENSE)으로 배포됩니다.
