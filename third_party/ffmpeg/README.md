# FFmpeg runtime binary

`ffmpeg` is built from FFmpeg 9.0.2 for the Vercel Linux x86_64 runtime. The
build enables only the demuxers, decoders, filters, encoder, and pipe protocol
used to sample Telegram GIF animations and WEBM stickers. It disables GPL,
non-free, version 3, autodetected libraries, and network protocols. The LGPL
license text is in `COPYING.LGPLv2.1`.

The source archive is <https://ffmpeg.org/releases/ffmpeg-9.0.2.tar.xz> with
SHA-256 `8c3850283eb25fa026482078a04051e0be17347b09ef81a0849bec15a96e002e`.
Run `./third_party/ffmpeg/build.sh` to download the pinned source, verify its
checksum, and rebuild the binary. To rebuild without network access, pass the
same source archive as the first argument. `embed.go` embeds this file into the
Go function build; when needed at runtime, the application writes it to the
writable `/tmp/yanineko` directory. Outside Vercel, the application first uses
a binary from the function directory or `PATH`, then the embedded copy on Linux
x86_64.
