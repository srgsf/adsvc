#!/usr/bin/env bash
# Builds the minimal ffmpeg the proxy needs, one static binary per target, into
# .cache/ffmpeg/bin/<os>_<arch>/ffmpeg. Runs inside the builder image (Dockerfile.builder).
# make ffmpeg copies the GOOS/GOARCH one to dist/; make dist bundles them all.
#
#   scripts/build-ffmpeg.sh [target...]    # default: all targets
#
# Targets: linux_amd64 linux_arm64 linux_armv7
#          darwin_arm64 windows_amd64
#
# Linux builds are static (musl); macOS ones link libSystem only (zig ships its stubs, no
# SDK needed) and keep the linker's ad-hoc signature, which arm64 macOS requires.
#
# The proxy demuxes every container itself and hands ffmpeg only pre-framed audio on a
# pipe (see internal/decode), so this build has no container demuxers except the
# rewrap Matroska and the raw self-framed formats, no network, no video. Everything below
# is ffmpeg's own code, so the result is LGPL. The offline tools (enroll, scan, serve) read
# whole files and URLs and need a regular ffmpeg.
set -euo pipefail

FFMPEG_VERSION=${FFMPEG_VERSION:-9.0.2}
ROOT=$(cd "$(dirname "$0")/.." && pwd)
CACHE=${CACHE:-$ROOT/.cache/ffmpeg}
OUT=${OUT:-$ROOT/.cache/ffmpeg/bin}
WORK=${WORK:-/tmp/ffmpeg-build}
JOBS=${JOBS:-$(nproc)}
export ZIG_GLOBAL_CACHE_DIR=${ZIG_GLOBAL_CACHE_DIR:-$CACHE/zig}
export ZIG_LOCAL_CACHE_DIR=$ZIG_GLOBAL_CACHE_DIR

# Keep in sync with the ffmpeg arguments built in internal/decode and the format tables of
# internal/demux. TestDecodeFormats (make test-minimal) checks it.
PCM=pcm_s16le,pcm_s16be,pcm_s24le,pcm_s24be,pcm_s32le,pcm_s32be,pcm_u8,pcm_f32le,pcm_f64le
COMPONENTS=(
	--disable-everything
	--enable-protocol=pipe,file
	--enable-demuxer=matroska,mp3,ac3,eac3,dts,aac,loas,$PCM
	--enable-decoder=aac,aac_latm,mp1,mp1float,mp2,mp2float,mp3,mp3float,ac3,eac3,dca,opus,vorbis,flac,alac,$PCM
	--enable-parser=aac,aac_latm,ac3,dca,mpegaudio,opus,vorbis,flac
	--enable-filter=aresample,aformat,anull,atrim
	--enable-muxer=pcm_s16le
	--enable-encoder=pcm_s16le
)
COMMON=(
	--enable-cross-compile --pkg-config=false
	--disable-autodetect --disable-network --disable-doc --disable-debug
	--disable-avdevice --disable-swscale --disable-ffplay --disable-ffprobe
	--enable-small --enable-static --disable-shared --disable-stripping
	--ar=llvm-ar --nm=llvm-nm --ranlib=llvm-ranlib
)

# zig cc as the cross-compiler: configure wants a single executable for --cc. zig's linker
# rejects a few flags configure adds, and zig does the same by default, so the wrapper drops
# them: -dynamic,-search_paths_first (Darwin: ld64 defaults), --pic-executable with the
# mainCRTStartup entry and the high image base (Windows: PE32+ defaults).
zigcc() {
	local f=$WORK/bin/zigcc-$1
	mkdir -p "$WORK/bin"
	cat >"$f" <<EOF
#!/bin/sh
for a; do
	shift
	case "\$a" in
	-Wl,-dynamic,-search_paths_first | -Wl,--pic-executable,-e,mainCRTStartup | -Wl,--image-base,0x140000000) ;;
	*) set -- "\$@" "\$a" ;;
	esac
done
exec zig cc -target $1 ${2:-} "\$@"
EOF
	chmod +x "$f"
	echo "$f"
}

target_args() {
	case $1 in
	linux_amd64) echo --target-os=linux --arch=x86_64 --cc="$(zigcc x86_64-linux-musl)" --extra-ldflags=-static ;;
	linux_arm64) echo --target-os=linux --arch=aarch64 --cc="$(zigcc aarch64-linux-musl)" --extra-ldflags=-static ;;
	linux_armv7) echo --target-os=linux --arch=arm --cc="$(zigcc arm-linux-musleabihf -mcpu=generic+v7a+vfp3d16)" --extra-ldflags=-static ;;
	darwin_arm64) echo --target-os=darwin --arch=aarch64 --cc="$(zigcc aarch64-macos)" ;;
	windows_amd64) echo --target-os=mingw32 --arch=x86_64 --cc="$(zigcc x86_64-windows-gnu)" --extra-ldflags=-static ;;
	*)
		echo "unknown target $1" >&2
		return 1
		;;
	esac
}

src=$CACHE/ffmpeg-$FFMPEG_VERSION.tar.xz
if [ ! -f "$src" ]; then
	mkdir -p "$CACHE"
	wget -q -O "$src.tmp" "https://ffmpeg.org/releases/ffmpeg-$FFMPEG_VERSION.tar.xz"
	mv "$src.tmp" "$src"
fi

# A stamp of everything that affects the result: skip targets that are already built.
stamp=$(printf '%s\n' "$FFMPEG_VERSION" "${COMPONENTS[@]}" "${COMMON[@]}" | sha256sum | cut -c1-16)

targets=("$@")
[ ${#targets[@]} -gt 0 ] || targets=(linux_amd64 linux_arm64 linux_armv7 darwin_arm64 windows_amd64)
for t in "${targets[@]}"; do
	dst=$OUT/$t
	exe=
	[[ $t != windows_* ]] || exe=.exe
	if [ -f "$dst/ffmpeg$exe" ] && [ "$(cat "$dst/.stamp" 2>/dev/null)" = "$stamp" ]; then
		echo "ffmpeg $t: up to date"
		continue
	fi
	echo "ffmpeg $t: building $FFMPEG_VERSION"
	args=$(target_args "$t")
	dir=$WORK/$t
	rm -rf "$dir" && mkdir -p "$dir"
	tar -xJf "$src" -C "$dir" --strip-components=1
	(
		cd "$dir"
		# shellcheck disable=SC2086 # $args is a list of words without spaces
		./configure $args "${COMMON[@]}" "${COMPONENTS[@]}" >configure.log 2>&1 ||
			{ tail -20 configure.log ffbuild/config.log; exit 1; }
		make -j"$JOBS" "ffmpeg$exe" >make.log 2>&1 || { tail -40 make.log; exit 1; }
	)
	mkdir -p "$dst"
	# llvm-strip re-signs linker-signed (ad-hoc) Mach-O binaries, so macOS arm64 still runs them
	llvm-strip -o "$dst/ffmpeg$exe" "$dir/ffmpeg_g$exe"
	cp "$dir/COPYING.LGPLv2.1" "$dst/ffmpeg-LICENSE"
	echo "$stamp" >"$dst/.stamp"
	file -b "$dst/ffmpeg$exe"
	ls -l "$dst/ffmpeg$exe"
done
