#!/bin/sh
# Deterministic build driver for GVX Desktop releases and local verification.
#
# Usage:
#   release/repro-build.sh install-ggui [BINDIR]
#   release/repro-build.sh binary GOOS GOARCH VERSION OUTPUT
#   release/repro-build.sh package TARGET ARCH VERSION OUTDIR [GGUI_FLAGS...]
#   release/repro-build.sh verify-binary BINARY GOOS GOARCH
#
# Reads pinned toolchain settings from release/toolchain.env, scrubs ambient
# environment variables, disables cgo for application builds, strips paths
# and build IDs, and normalizes archive timestamps to SOURCE_DATE_EPOCH.
set -eu

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
	sed -n "s/^$1 = \"\\(.*\\)\"\$/\\1/p" "$repo_root/ggui.toml"
}

app_name=$(field name)
app_id=$(field id)
[ -n "$app_name" ] || { echo "repro-build: ggui.toml has no app name" >&2; exit 1; }
[ -n "$app_id" ] || { echo "repro-build: ggui.toml has no app id" >&2; exit 1; }

resolve_epoch() {
	if [ -n "${SOURCE_DATE_EPOCH:-}" ]; then
		printf '%s\n' "$SOURCE_DATE_EPOCH"
		return
	fi
	git -C "$repo_root" log -1 --format=%ct
}

set_mtime() {
	epoch=$1
	target=$2
	if touch -d "@$epoch" "$target" 2>/dev/null; then
		return
	fi
	stamp=$(TZ=UTC date -r "$epoch" +%Y%m%d%H%M.%S)
	touch -t "$stamp" "$target"
}

set_tree_mtime() {
	epoch=$1
	root=$2
	find "$root" | while IFS= read -r item; do
		set_mtime "$epoch" "$item"
	done
}

pack_linux_tar() {
	tree=$1
	out=$2
	epoch=$3
	tar_cmd=tar
	if ! tar --version 2>/dev/null | grep -qi 'GNU tar'; then
		if command -v gtar >/dev/null 2>&1; then
			tar_cmd=gtar
		fi
	fi
	rm -f "$out"
	if "$tar_cmd" --version 2>/dev/null | grep -qi 'GNU tar'; then
		"$tar_cmd" \
			--sort=name \
			--mtime="@$epoch" \
			--owner=0 \
			--group=0 \
			--numeric-owner \
			--format=ustar \
			-C "$tree" \
			-cf - usr | gzip -9n > "$out"
	else
		set_tree_mtime "$epoch" "$tree/usr"
		(
			cd "$tree"
			LC_ALL=C find usr | LC_ALL=C sort | \
				tar -n --uid 0 --gid 0 --uname root --gname root --format ustar -T - -cf -
		) | gzip -9n > "$out"
	fi
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
		"$go_bin" install \
			-trimpath \
			-pgo=off \
			-compiler=gc \
			-ldflags="-buildid= -s -w" \
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
	ggui_bin=${GGUI:-$(command -v ggui 2>/dev/null || printf '%s/bin/ggui' "$("$go_bin" env GOPATH)")}
	[ -x "$ggui_bin" ] || { echo "repro-build: ggui not found at $ggui_bin; run install-ggui first" >&2; exit 1; }
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
		set_mtime "$epoch" "$exe"
		zip="gvx-desktop-$version-windows-$arch.zip"
		rm -f "$outdir_abs/$zip"
		(
			cd "$outdir_abs"
			TZ=UTC zip -q -X "$zip" "$app_name.exe"
		)
		;;
	linux)
		(
			cd "$repo_root"
			run_ggui build --target linux --format tar,deb,rpm --out "$outdir_abs" --ldflags "-buildid= -X main.version=$version" "$@"
		)
		tree="$outdir_abs/gvx-desktop-linux-$arch"
		tarball="$outdir_abs/gvx-desktop-$version-linux-$arch.tar.gz"
		pack_linux_tar "$tree" "$tarball" "$epoch"
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
	bid=$(GOTOOLCHAIN="$toolchain" "$go_bin" tool buildid "$bin" 2>/dev/null || true)
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
