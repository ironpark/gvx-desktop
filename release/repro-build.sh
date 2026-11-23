#!/bin/sh
# Deterministic build driver for GVX Desktop releases and local verification.
#
# Usage:
#   release/repro-build.sh install-ggui [BINDIR]
#   release/repro-build.sh binary GOOS GOARCH VERSION OUTPUT
#   release/repro-build.sh package TARGET ARCH VERSION OUTDIR [GGUI_FLAGS...]
#   release/repro-build.sh verify-binary BINARY GOOS GOARCH
#
# Reads pinned toolchain settings from release/toolchain.env, normalizes umask,
# scrubs ambient environment variables, disables cgo for application builds,
# strips paths and build IDs, and packs Windows zips and Linux tarballs with
# release/repro-pack.go using SOURCE_DATE_EPOCH so archives match bit for bit
# across Linux and macOS hosts.
set -eu
umask 022

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
# shellcheck source=release/toolchain.env
. "$repo_root/release/toolchain.env"

go_bin=${GO:-go}
go_path=$(command -v "$go_bin") || { echo "repro-build: go not found on PATH" >&2; exit 1; }
gobin_dir=$(dirname -- "$go_path")
goproxy=${GVX_DESKTOP_GOPROXY:-$GOPROXY}
gosumdb=${GVX_DESKTOP_GOSUMDB:-$GOSUMDB}
toolchain="go$GO_VERSION"
gomodcache=${GOMODCACHE:-$("$go_bin" env GOMODCACHE)}
gocache=${GOCACHE:-$("$go_bin" env GOCACHE)}
home_dir=${HOME:-/tmp}
tmp_dir=${TMPDIR:-/tmp}

field() {
	awk -v key="$1" '
		/^\[app\][[:space:]]*$/ { in_app = 1; next }
		/^\[/ { in_app = 0 }
		in_app && index($0, key " = \"") == 1 {
			sub(/^[^"]*"/, "")
			sub(/"[[:space:]]*$/, "")
			print
			exit
		}
	' "$repo_root/ggui.toml"
}

app_name=$(field name)
app_id=$(field id)
app_version=$(field version)
[ -n "$app_name" ] || { echo "repro-build: ggui.toml has no [app] name" >&2; exit 1; }
[ -n "$app_id" ] || { echo "repro-build: ggui.toml has no [app] id" >&2; exit 1; }
[ -n "$app_version" ] || { echo "repro-build: ggui.toml has no [app] version" >&2; exit 1; }

resolve_epoch() {
	if [ -n "${SOURCE_DATE_EPOCH:-}" ]; then
		printf '%s\n' "$SOURCE_DATE_EPOCH"
		return
	fi
	git -C "$repo_root" log -1 --format=%ct
}

run_repro_pack() {
	env -i \
		PATH="$gobin_dir:/usr/local/bin:/usr/bin:/bin" \
		HOME="$home_dir" \
		TMPDIR="$tmp_dir" \
		LC_ALL=C \
		TZ=UTC \
		GOMODCACHE="$gomodcache" \
		GOCACHE="$gocache" \
		GOENV=off \
		GOWORK=off \
		GO111MODULE=on \
		GOPROXY="$goproxy" \
		GOSUMDB="$gosumdb" \
		GOTOOLCHAIN="$toolchain" \
		GOTELEMETRY=off \
		GOFIPS140=off \
		GOEXPERIMENT= \
		GODEBUG= \
		GOFLAGS= \
		CGO_ENABLED=0 \
		${SYSTEMROOT:+SYSTEMROOT="$SYSTEMROOT"} \
		${USERPROFILE:+USERPROFILE="$USERPROFILE"} \
		${LOCALAPPDATA:+LOCALAPPDATA="$LOCALAPPDATA"} \
		"$go_bin" run "$repo_root/release/repro-pack.go" "$@"
}

cmd=${1:?usage: repro-build.sh <install-ggui|binary|package|verify-binary> ...}
shift

case "$cmd" in
install-ggui)
	bindir=${1:-$("$go_bin" env GOPATH)/bin}
	mkdir -p -- "$bindir"
	bindir_abs=$(CDPATH= cd -- "$bindir" && pwd)
	ggui_cgo=${GGUI_CGO:-0}
	exec env -i \
		PATH="$gobin_dir:/usr/local/bin:/usr/bin:/bin" \
		HOME="$home_dir" \
		TMPDIR="$tmp_dir" \
		LC_ALL=C \
		TZ=UTC \
		GOMODCACHE="$gomodcache" \
		GOCACHE="$gocache" \
		GOBIN="$bindir_abs" \
		GOENV=off \
		GOWORK=off \
		GO111MODULE=on \
		GOPROXY="$goproxy" \
		GOSUMDB="$gosumdb" \
		GOTOOLCHAIN="$toolchain" \
		GOTELEMETRY=off \
		GOFIPS140=off \
		GOEXPERIMENT= \
		GODEBUG= \
		GOFLAGS= \
		CGO_ENABLED="$ggui_cgo" \
		${SYSTEMROOT:+SYSTEMROOT="$SYSTEMROOT"} \
		${USERPROFILE:+USERPROFILE="$USERPROFILE"} \
		${LOCALAPPDATA:+LOCALAPPDATA="$LOCALAPPDATA"} \
		"$go_bin" install \
			-trimpath \
			-pgo=off \
			-compiler=gc \
			-ldflags="-s -w -buildid=" \
			-- "github.com/ironpark/ggui/cmd/ggui@$GGUI_VERSION"
	;;

binary)
	goos=${1:?usage: repro-build.sh binary GOOS GOARCH VERSION OUTPUT}
	goarch=${2:?usage: repro-build.sh binary GOOS GOARCH VERSION OUTPUT}
	version=${3:?usage: repro-build.sh binary GOOS GOARCH VERSION OUTPUT}
	out=${4:?usage: repro-build.sh binary GOOS GOARCH VERSION OUTPUT}
	mkdir -p -- "$(dirname -- "$out")"
	out_abs="$(CDPATH= cd -- "$(dirname -- "$out")" && pwd)/$(basename -- "$out")"
	ldflags="-s -w -buildid= -X main.version=$version -X github.com/ironpark/ggui.buildID=$app_id"
	if [ "$goos" = windows ] && [ "${WINDOWS_GUI:-0}" = 1 ]; then
		ldflags="$ldflags -H=windowsgui"
	fi
	exec env -i \
		PATH="$gobin_dir:/usr/local/bin:/usr/bin:/bin" \
		HOME="$home_dir" \
		TMPDIR="$tmp_dir" \
		LC_ALL=C \
		TZ=UTC \
		GOMODCACHE="$gomodcache" \
		GOCACHE="$gocache" \
		GOENV=off \
		GOWORK=off \
		GO111MODULE=on \
		GOPROXY="$goproxy" \
		GOSUMDB="$gosumdb" \
		GOTOOLCHAIN="$toolchain" \
		GOTELEMETRY=off \
		GOFIPS140=off \
		GOEXPERIMENT= \
		GODEBUG= \
		GOFLAGS= \
		CGO_ENABLED=0 \
		GOOS="$goos" \
		GOARCH="$goarch" \
		GOAMD64="$GOAMD64" \
		GOARM64="$GOARM64" \
		${SYSTEMROOT:+SYSTEMROOT="$SYSTEMROOT"} \
		${USERPROFILE:+USERPROFILE="$USERPROFILE"} \
		${LOCALAPPDATA:+LOCALAPPDATA="$LOCALAPPDATA"} \
		"$go_bin" build \
			-C "$repo_root" \
			-mod=readonly \
			-pgo=off \
			-buildvcs=false \
			-trimpath \
			-buildmode=exe \
			-compiler=gc \
			-ldflags="$ldflags" \
			-o "$out_abs" \
			.
	;;

package)
	target=${1:?usage: repro-build.sh package TARGET ARCH VERSION OUTDIR [GGUI_FLAGS...]}
	arch=${2:?usage: repro-build.sh package TARGET ARCH VERSION OUTDIR [GGUI_FLAGS...]}
	version=${3:?usage: repro-build.sh package TARGET ARCH VERSION OUTDIR [GGUI_FLAGS...]}
	outdir=${4:?usage: repro-build.sh package TARGET ARCH VERSION OUTDIR [GGUI_FLAGS...]}
	shift 4

	if [ "$version" != "$app_version" ]; then
		echo "repro-build: version argument $version does not match ggui.toml ($app_version)" >&2
		exit 1
	fi

	if [ -n "${GGUI:-}" ]; then
		ggui_bin=$GGUI
	elif [ -x "$("$go_bin" env GOPATH)/bin/ggui" ]; then
		ggui_bin="$("$go_bin" env GOPATH)/bin/ggui"
	else
		ggui_bin=$(command -v ggui 2>/dev/null || true)
	fi
	[ -n "$ggui_bin" ] && [ -x "$ggui_bin" ] || {
		echo "repro-build: ggui not found; run ./release/repro-build.sh install-ggui first" >&2
		exit 1
	}

	ggui_meta=$(GOTOOLCHAIN="$toolchain" "$go_bin" version -m "$ggui_bin")
	if ! printf '%s\n' "$ggui_meta" | grep -Eq "^[[:space:]]+mod[[:space:]]+github\.com/ironpark/ggui/cmd/ggui[[:space:]]+$GGUI_VERSION([[:space:]]|\$)"; then
		echo "repro-build: $ggui_bin does not match pinned ggui $GGUI_VERSION; run ./release/repro-build.sh install-ggui" >&2
		exit 1
	fi

	ggui_dir=$(dirname -- "$ggui_bin")
	epoch=$(resolve_epoch)
	mkdir -p -- "$outdir"
	outdir_abs=$(CDPATH= cd -- "$outdir" && pwd)

	run_ggui() {
		env -i \
			PATH="$ggui_dir:$gobin_dir:/usr/local/bin:/usr/bin:/bin" \
			HOME="$home_dir" \
			TMPDIR="$tmp_dir" \
			LC_ALL=C \
			TZ=UTC \
			SOURCE_DATE_EPOCH="$epoch" \
			GOMODCACHE="$gomodcache" \
			GOCACHE="$gocache" \
			GOENV=off \
			GOWORK=off \
			GO111MODULE=on \
			GOPROXY="$goproxy" \
			GOSUMDB="$gosumdb" \
			GOTOOLCHAIN="$toolchain" \
			GOTELEMETRY=off \
			GOFIPS140=off \
			GOEXPERIMENT= \
			GODEBUG= \
			GOFLAGS="-mod=readonly -pgo=off -buildvcs=false" \
			CGO_ENABLED=0 \
			GOARCH="$arch" \
			GOAMD64="$GOAMD64" \
			GOARM64="$GOARM64" \
			${SYSTEMROOT:+SYSTEMROOT="$SYSTEMROOT"} \
			${USERPROFILE:+USERPROFILE="$USERPROFILE"} \
			${LOCALAPPDATA:+LOCALAPPDATA="$LOCALAPPDATA"} \
			${GGUI_P12_BASE64:+GGUI_P12_BASE64="$GGUI_P12_BASE64"} \
			${GGUI_P12_PASSWORD:+GGUI_P12_PASSWORD="$GGUI_P12_PASSWORD"} \
			${GGUI_NOTARY_PASSWORD:+GGUI_NOTARY_PASSWORD="$GGUI_NOTARY_PASSWORD"} \
			"$ggui_bin" "$@"
	}

	case "$target" in
	macos)
		if [ "$(basename -- "$repo_root")" != "gvx-desktop" ]; then
			echo "repro-build: warning: repo directory is $(basename -- "$repo_root"), not gvx-desktop; CFBundleExecutable follows the directory name" >&2
		fi
		(
			cd "$repo_root"
			run_ggui build --target macos --dmg --out "$outdir_abs" --ldflags "-buildid= -X main.version=$version" "$@"
			"$repo_root/release/check-bundle.sh" "$outdir_abs"
		)
		dmg="gvx-desktop-$version-macos-$arch.dmg"
		mv -f "$outdir_abs/$app_name.dmg" "$outdir_abs/$dmg"
		;;
	windows)
		(
			cd "$repo_root"
			run_ggui build --target windows --out "$outdir_abs" --ldflags "-buildid= -X main.version=$version" "$@"
		)
		exe="$outdir_abs/$app_name.exe"
		zip="$outdir_abs/gvx-desktop-$version-windows-$arch.zip"
		rm -f "$zip"
		run_repro_pack zip "$epoch" "$zip" "$exe"
		;;
	linux)
		(
			cd "$repo_root"
			run_ggui build --target linux --format tar,deb,rpm --out "$outdir_abs" --ldflags "-buildid= -X main.version=$version" "$@"
		)
		tree="$outdir_abs/gvx-desktop-linux-$arch"
		tarball="$outdir_abs/gvx-desktop-$version-linux-$arch.tar.gz"
		rm -f "$tarball"
		run_repro_pack tar "$epoch" "$tarball" "$tree"
		;;
	*)
		echo "repro-build: unknown target '$target' (expected macos, windows, or linux)" >&2
		exit 1
		;;
	esac
	;;

verify-binary)
	bin=${1:?usage: repro-build.sh verify-binary BINARY GOOS GOARCH}
	want_os=${2:?usage: repro-build.sh verify-binary BINARY GOOS GOARCH}
	want_arch=${3:?usage: repro-build.sh verify-binary BINARY GOOS GOARCH}
	meta=$(GOTOOLCHAIN="$toolchain" "$go_bin" version -m "$bin")
	fail() {
		echo "repro-build: verify-binary failed for $bin: $1" >&2
		printf '%s\n' "$meta" >&2
		exit 1
	}
	printf '%s\n' "$meta" | head -n 1 | grep -Fq "$toolchain" || fail "expected toolchain $toolchain"
	printf '%s\n' "$meta" | grep -Fq -- "-buildmode=exe" || fail "expected -buildmode=exe"
	printf '%s\n' "$meta" | grep -Fq -- "-compiler=gc" || fail "expected -compiler=gc"
	printf '%s\n' "$meta" | grep -Fq -- "-trimpath=true" || fail "expected -trimpath=true"
	printf '%s\n' "$meta" | grep -Fq "CGO_ENABLED=0" || fail "expected CGO_ENABLED=0"
	printf '%s\n' "$meta" | grep -Fq "GOOS=$want_os" || fail "expected GOOS=$want_os"
	printf '%s\n' "$meta" | grep -Fq "GOARCH=$want_arch" || fail "expected GOARCH=$want_arch"
	if [ "$want_arch" = amd64 ]; then
		printf '%s\n' "$meta" | grep -Fq "GOAMD64=$GOAMD64" || fail "expected GOAMD64=$GOAMD64"
	elif [ "$want_arch" = arm64 ]; then
		printf '%s\n' "$meta" | grep -Fq "GOARM64=$GOARM64" || fail "expected GOARM64=$GOARM64"
	fi
	if printf '%s\n' "$meta" | grep -Eq '^[[:space:]]+build[[:space:]]+vcs\.'; then
		fail "unexpected VCS metadata embedded in binary"
	fi
	bid=$(GOTOOLCHAIN="$toolchain" "$go_bin" tool buildid "$bin")
	if [ -n "$bid" ]; then
		fail "expected empty Go build ID (-buildid=), got '$bid'"
	fi
	echo "verify-binary: $bin ($toolchain $want_os/$want_arch, buildid=empty)"
	;;

*)
	echo "repro-build: unknown command '$cmd'" >&2
	exit 1
	;;
esac
