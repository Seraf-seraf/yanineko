// Package ffmpeg contains the pinned FFmpeg executable used by Vercel builds.
package ffmpeg

import _ "embed"

// Binary is the statically linked LGPL FFmpeg executable for Linux x86_64.
//
//go:embed ffmpeg
var Binary []byte
