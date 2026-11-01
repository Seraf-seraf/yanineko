#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
build_root=$(mktemp -d)
trap 'rm -rf "$build_root"' EXIT

version=9.0.2
source_sha256=8c3850283eb25fa026482078a04051e0be17347b09ef81a0849bec15a96e002e
archive="$build_root/ffmpeg-$version.tar.xz"
source_url="https://ffmpeg.org/releases/ffmpeg-$version.tar.xz"

if [[ -n "${1:-}" ]]; then
	cp -- "$1" "$archive"
else
	curl -fsSL --connect-timeout 10 --max-time 120 "$source_url" -o "$archive"
fi
printf '%s  %s\n' "$source_sha256" "$archive" | sha256sum --check -
tar -xf "$archive" -C "$build_root"
source_dir="$build_root/ffmpeg-$version"

(
	cd "$source_dir"
	./configure \
		--prefix=/usr \
		--disable-autodetect \
		--disable-everything \
		--disable-programs \
		--enable-ffmpeg \
		--disable-network \
		--disable-doc \
		--disable-debug \
		--disable-gpl \
		--disable-nonfree \
		--disable-version3 \
		--enable-small \
		--disable-x86asm \
		--enable-demuxer=mov,matroska,gif \
		--enable-decoder=h264,vp8,vp9,av1,gif,mjpeg,png \
		--enable-parser=h264,vp8,vp9,av1 \
		--enable-protocol=pipe \
		--enable-filter=fps,scale \
		--enable-encoder=mjpeg \
		--enable-muxer=image2pipe \
		--enable-static \
		--disable-shared \
		--extra-ldflags=-static \
		--extra-libs='-lpthread -lm' > "$build_root/configure.log"
	grep -F 'License: LGPL version 2.1 or later' "$build_root/configure.log"
	make -j2 ffmpeg
)

built_binary="$source_dir/ffmpeg"
version_output=$("$built_binary" -version 2>&1)
configuration=$(printf '%s\n' "$version_output" | sed -n 's/^configuration: //p')
if [[ -z "$configuration" ]]; then
	printf 'FFmpeg did not report its build configuration\n' >&2
	exit 1
fi
case "$configuration" in
*--enable-gpl*|*--enable-nonfree*|*--enable-version3*)
	printf 'FFmpeg build unexpectedly enables a restricted license flag\n' >&2
	exit 1
	;;
esac
for required_flag in --disable-gpl --disable-nonfree --disable-version3 --disable-network --enable-protocol=pipe; do
	case "$configuration" in
	*"$required_flag"*) ;;
	*)
		printf 'FFmpeg build is missing required flag %s\n' "$required_flag" >&2
		exit 1
		;;
	esac
done
printf '%s\n' "$version_output" | grep -Fq "ffmpeg version $version"
decoder_list=$("$built_binary" -decoders 2>&1)
filter_list=$("$built_binary" -filters 2>&1)
encoder_list=$("$built_binary" -encoders 2>&1)
grep -Eq '[[:space:]]h264[[:space:]]' <<< "$decoder_list"
grep -Eq '[[:space:]]vp9[[:space:]]' <<< "$decoder_list"
grep -Eq '[[:space:]]fps[[:space:]]' <<< "$filter_list"
grep -Eq '[[:space:]]scale[[:space:]]' <<< "$filter_list"
grep -Eq '[[:space:]]mjpeg[[:space:]]' <<< "$encoder_list"

install -m 0755 "$built_binary" "$script_dir/ffmpeg"
install -m 0644 "$source_dir/COPYING.LGPLv2.1" "$script_dir/COPYING.LGPLv2.1"
printf 'Built LGPL FFmpeg %s at %s\n' "$version" "$script_dir/ffmpeg"
