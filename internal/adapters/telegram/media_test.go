package telegram

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestLoadMediaDownloadsAndNormalizesLargestPhoto(t *testing.T) {
	var imageData bytes.Buffer
	if err := png.Encode(&imageData, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bot123:synthetic-token/getFile":
			if r.Method != http.MethodPost {
				t.Errorf("getFile method = %s, want POST", r.Method)
			}
			var request struct {
				FileID string `json:"file_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.FileID != "large-photo" {
				t.Errorf("getFile request = %#v, %v", request, err)
			}
			_, _ = w.Write([]byte(`{"ok":true,"result":{"file_path":"photos/p.png","file_size":100}}`))
		case "/file/bot123:synthetic-token/photos/p.png":
			_, _ = w.Write(imageData.Bytes())
		default:
			t.Errorf("unexpected Telegram path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := newClient("123:synthetic-token", 123, server.URL, server.Client())
	update := Update{ID: 1, Message: &Message{ID: 2, Chat: Chat{ID: 3, Type: "private"}, Photo: []PhotoSize{
		{FileID: "small-photo", Width: 32, Height: 32},
		{FileID: "large-photo", Width: 64, Height: 64},
	}}}
	media, err := client.LoadMedia(context.Background(), update)
	if err != nil {
		t.Fatalf("LoadMedia() error = %v", err)
	}
	if len(media.Images) != 1 || media.Images[0].MIMEType != "image/png" || media.Source != MediaSourceOriginal {
		t.Fatalf("LoadMedia() = %#v, want original normalized PNG", media)
	}
	if _, format, err := image.Decode(bytes.NewReader(media.Images[0].Data)); err != nil || format != "png" {
		t.Fatalf("normalized frame format = %q, %v, want PNG", format, err)
	}
}

func TestLoadMediaExtractsEverySampledTelegramAnimationFrame(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("bundled FFmpeg targets Linux x86_64")
	}
	animation, err := os.ReadFile(filepath.Join("testdata", "tiny-h264-animation.bin"))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bot123:synthetic-token/getFile":
			_, _ = w.Write([]byte(`{"ok":true,"result":{"file_path":"animations/clip.mp4","file_size":` + strconv.Itoa(len(animation)) + `}}`))
		case "/file/bot123:synthetic-token/animations/clip.mp4":
			_, _ = w.Write(animation)
		default:
			t.Errorf("unexpected Telegram path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newClient("123:synthetic-token", 123, server.URL, server.Client())
	update := Update{ID: 10, Message: &Message{ID: 11, Chat: Chat{ID: 12, Type: "private"}, Animation: &MediaFile{
		FileID: "telegram-animation", FileSize: int64(len(animation)), MIMEType: "video/mp4",
	}}}
	media, err := client.LoadMedia(context.Background(), update)
	if err != nil {
		t.Fatalf("LoadMedia() error = %v", err)
	}
	if len(media.Images) != MaxVisionFrames || media.Source != MediaSourceOriginal {
		t.Fatalf("LoadMedia() = %#v, want %d frames from original animation", media, MaxVisionFrames)
	}
	for index, frame := range media.Images {
		if frame.MIMEType != "image/png" {
			t.Errorf("frame %d MIMEType = %q, want image/png", index, frame.MIMEType)
		}
		if _, format, err := image.Decode(bytes.NewReader(frame.Data)); err != nil || format != "png" {
			t.Errorf("frame %d format = %q, %v, want valid PNG", index, format, err)
		}
	}
}

func TestLoadMediaReportsThumbnailFallbackSource(t *testing.T) {
	var thumbnail bytes.Buffer
	if err := png.Encode(&thumbnail, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bot123:synthetic-token/getFile":
			var request struct {
				FileID string `json:"file_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode getFile request: %v", err)
			}
			if request.FileID == "broken-animation" {
				_, _ = w.Write([]byte(`{"ok":true,"result":{"file_path":"animations/broken.mp4","file_size":6}}`))
			} else if request.FileID == "animation-thumbnail" {
				_, _ = w.Write([]byte(`{"ok":true,"result":{"file_path":"photos/thumb.png","file_size":` + strconv.Itoa(thumbnail.Len()) + `}}`))
			} else {
				t.Errorf("unexpected Telegram file ID %q", request.FileID)
				http.NotFound(w, r)
			}
		case "/file/bot123:synthetic-token/animations/broken.mp4":
			_, _ = w.Write([]byte("broken"))
		case "/file/bot123:synthetic-token/photos/thumb.png":
			_, _ = w.Write(thumbnail.Bytes())
		default:
			t.Errorf("unexpected Telegram path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newClient("123:synthetic-token", 123, server.URL, server.Client())
	update := Update{ID: 13, Message: &Message{ID: 14, Chat: Chat{ID: 15, Type: "private"}, Animation: &MediaFile{
		FileID: "broken-animation", FileSize: 6, MIMEType: "video/mp4",
		Thumbnail: &PhotoSize{FileID: "animation-thumbnail", FileSize: int64(thumbnail.Len())},
	}}}
	media, err := client.LoadMedia(context.Background(), update)
	if err != nil {
		t.Fatalf("LoadMedia() error = %v", err)
	}
	if len(media.Images) != 1 || media.Source != MediaSourceThumbnail {
		t.Fatalf("LoadMedia() = %#v, want one fallback thumbnail", media)
	}
}

func TestDecodeMediaFramesReturnsSeveralGIFFrames(t *testing.T) {
	palette := color.Palette{color.Transparent, color.RGBA{R: 255, A: 255}}
	first := image.NewPaletted(image.Rect(0, 0, 2, 2), palette)
	second := image.NewPaletted(image.Rect(0, 0, 2, 2), palette)
	second.SetColorIndex(0, 0, 1)
	var data bytes.Buffer
	if err := gif.EncodeAll(&data, &gif.GIF{Image: []*image.Paletted{first, second}, Delay: []int{10, 10}, Config: image.Config{Width: 2, Height: 2, ColorModel: palette}}); err != nil {
		t.Fatal(err)
	}
	frames, err := decodeMediaFrames(context.Background(), "animation", data.Bytes())
	if err != nil {
		t.Fatalf("decodeMediaFrames() error = %v", err)
	}
	if len(frames) != 2 {
		t.Fatalf("decoded frames = %d, want 2", len(frames))
	}
}

func TestDecodeVideoFramesUsesBundledFFmpegForTelegramAnimations(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("bundled FFmpeg targets Linux x86_64")
	}
	functionRoot, err := filepath.Abs("../../../")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LAMBDA_TASK_ROOT", functionRoot)
	t.Setenv("PATH", t.TempDir())

	ffmpeg, err := findFFmpeg(ffmpegBundleRoots())
	if err != nil {
		t.Fatalf("findFFmpeg() error = %v", err)
	}
	wantFFmpeg := filepath.Join(functionRoot, bundledFFmpegPath)
	if ffmpeg != wantFFmpeg {
		t.Fatalf("findFFmpeg() = %q, want bundled binary %q", ffmpeg, wantFFmpeg)
	}

	fixtures := []struct {
		name string
		kind string
	}{
		{name: "tiny-h264-animation.bin", kind: "animation"},
		{name: "tiny-vp9-sticker.bin", kind: "video"},
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", fixture.name))
			if err != nil {
				t.Fatal(err)
			}
			frames, err := decodeMediaFrames(context.Background(), fixture.kind, data)
			if err != nil {
				t.Fatalf("decodeMediaFrames() error = %v", err)
			}
			if len(frames) != MaxVisionFrames {
				t.Fatalf("decoded frames = %d, want %d", len(frames), MaxVisionFrames)
			}
			for index, frame := range frames {
				decoded, format, err := image.Decode(bytes.NewReader(frame))
				if err != nil || format != "png" || decoded.Bounds().Dx() > MaxVisionDimension || decoded.Bounds().Dy() > MaxVisionDimension {
					t.Errorf("frame %d = %s %v %#v, want bounded PNG", index, format, err, decoded)
				}
			}
		})
	}
}

func TestDecodeVideoFramesSamplesShortAnimationAtMoreThanOneFrame(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("bundled FFmpeg targets Linux x86_64")
	}
	data, err := os.ReadFile(filepath.Join("testdata", "tiny-short-animation.bin"))
	if err != nil {
		t.Fatal(err)
	}
	frames, err := decodeMediaFrames(context.Background(), "animation", data)
	if err != nil {
		t.Fatalf("decodeMediaFrames() error = %v", err)
	}
	if len(frames) != 2 {
		t.Fatalf("decoded frames = %d, want 2 samples from 500ms animation", len(frames))
	}
}

func TestEncodeImageFramesEvenlySamplesVideoCandidates(t *testing.T) {
	candidates := make([]image.Image, MaxVideoSampleFrames)
	for index := range candidates {
		frame := image.NewRGBA(image.Rect(0, 0, 1, 1))
		frame.SetRGBA(0, 0, color.RGBA{R: byte(index), A: 255})
		candidates[index] = frame
	}
	frames, err := encodeImageFrames(candidates)
	if err != nil {
		t.Fatalf("encodeImageFrames() error = %v", err)
	}
	if len(frames) != MaxVisionFrames {
		t.Fatalf("encoded frame count = %d, want %d", len(frames), MaxVisionFrames)
	}
	wantIndices := []byte{0, 3, 7, 11}
	for index, frameData := range frames {
		frame, _, err := image.Decode(bytes.NewReader(frameData))
		if err != nil {
			t.Fatalf("decode sampled frame %d: %v", index, err)
		}
		got := color.RGBAModel.Convert(frame.At(0, 0)).(color.RGBA).R
		if got != wantIndices[index] {
			t.Errorf("sampled frame %d red value = %d, want source candidate %d", index, got, wantIndices[index])
		}
	}
}

func TestDecodeVideoFramesUsesEmbeddedFFmpegWhenBundleFileIsUnavailable(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("embedded FFmpeg targets Linux x86_64")
	}
	bundleRoot := t.TempDir()
	temporaryRoot := t.TempDir()
	t.Setenv("TMPDIR", temporaryRoot)
	t.Setenv("PATH", t.TempDir())

	data, err := os.ReadFile(filepath.Join("testdata", "tiny-vp9-sticker.bin"))
	if err != nil {
		t.Fatal(err)
	}
	frames, err := decodeVideoFramesWithRoots(context.Background(), data, []string{bundleRoot})
	if err != nil {
		t.Fatalf("decodeVideoFramesWithRoots() error = %v", err)
	}
	if len(frames) != MaxVisionFrames {
		t.Fatalf("decoded frames = %d, want %d", len(frames), MaxVisionFrames)
	}

	ffmpeg := filepath.Join(temporaryRoot, "yanineko", "ffmpeg-9.0.2")
	info, err := os.Stat(ffmpeg)
	if err != nil {
		t.Fatalf("embedded FFmpeg stat error = %v", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("embedded FFmpeg mode = %v; want executable regular file", info.Mode())
	}
	if _, err := os.Stat(filepath.Join(bundleRoot, bundledFFmpegPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("bundle FFmpeg stat error = %v, want file absent", err)
	}
}

func TestDecodeGIFFramesCompositesPartialFrames(t *testing.T) {
	palette := color.Palette{color.Transparent, color.RGBA{G: 255, A: 255}, color.RGBA{R: 255, A: 255}}
	first := image.NewPaletted(image.Rect(0, 0, 2, 2), palette)
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			first.SetColorIndex(x, y, 1)
		}
	}
	second := image.NewPaletted(image.Rect(1, 1, 2, 2), palette)
	second.SetColorIndex(1, 1, 2)
	var data bytes.Buffer
	if err := gif.EncodeAll(&data, &gif.GIF{Image: []*image.Paletted{first, second}, Delay: []int{10, 10}, Config: image.Config{Width: 2, Height: 2, ColorModel: palette}}); err != nil {
		t.Fatal(err)
	}

	frames, err := decodeGIFFrames(data.Bytes())
	if err != nil {
		t.Fatalf("decodeGIFFrames() error = %v", err)
	}
	if len(frames) != 2 {
		t.Fatalf("decoded frames = %d, want 2", len(frames))
	}
	composite, format, err := image.Decode(bytes.NewReader(frames[1]))
	if err != nil || format != "png" || composite.Bounds() != image.Rect(0, 0, 2, 2) {
		t.Fatalf("second frame bounds/format = %v/%q, %v, want full 2x2 PNG", composite.Bounds(), format, err)
	}
	if got := color.RGBAModel.Convert(composite.At(0, 0)).(color.RGBA); got.G != 255 || got.A != 255 {
		t.Fatalf("unchanged first-frame pixel = %#v, want retained opaque green", got)
	}
	if got := color.RGBAModel.Convert(composite.At(1, 1)).(color.RGBA); got.R != 255 || got.A != 255 {
		t.Fatalf("partial second-frame pixel = %#v, want opaque red", got)
	}
}

func TestEncodeVisionFrameFitsOpenAIMediaBudget(t *testing.T) {
	frame := image.NewNRGBA(image.Rect(0, 0, MaxVisionDimension, MaxVisionDimension))
	state := uint32(1)
	for index := range frame.Pix {
		state = state*1664525 + 1013904223
		frame.Pix[index] = byte(state >> 24)
	}

	encoded, err := encodeVisionFrame(frame)
	if err != nil {
		t.Fatalf("encodeVisionFrame() error = %v", err)
	}
	if int64(len(encoded)) > MaxVisionFrameBytes {
		t.Fatalf("encoded frame = %d bytes, want at most %d", len(encoded), MaxVisionFrameBytes)
	}
	if _, format, err := image.Decode(bytes.NewReader(encoded)); err != nil || format != "png" {
		t.Fatalf("encoded frame format = %q, %v, want valid PNG", format, err)
	}
}

func TestDecodeMediaFramesRendersSeveralTGSFrames(t *testing.T) {
	const animation = `{"v":"5.7.4","fr":30,"ip":0,"op":30,"w":256,"h":256,"nm":"rect","ddd":0,"assets":[],"layers":[{"ddd":0,"ind":1,"ty":4,"nm":"rect","sr":1,"ks":{"o":{"a":0,"k":100},"r":{"a":1,"k":[{"t":0,"s":[0],"i":{"x":[1],"y":[1]},"o":{"x":[0],"y":[0]}},{"t":30,"s":[90]}]},"p":{"a":0,"k":[128,128,0]},"a":{"a":0,"k":[0,0,0]},"s":{"a":0,"k":[100,100,100]}},"ao":0,"shapes":[{"ty":"gr","it":[{"ty":"rc","d":1,"s":{"a":0,"k":[120,120]},"p":{"a":0,"k":[0,0]},"r":{"a":0,"k":12}},{"ty":"fl","c":{"a":0,"k":[1,0.3,0.1,1]},"o":{"a":0,"k":100}},{"ty":"tr","p":{"a":0,"k":[0,0]},"a":{"a":0,"k":[0,0]},"s":{"a":0,"k":[100,100]},"r":{"a":0,"k":0},"o":{"a":0,"k":100}}]}],"ip":0,"op":30,"st":0,"bm":0}]}`
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write([]byte(animation)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	frames, err := renderTGS(context.Background(), compressed.Bytes())
	if err != nil {
		t.Fatalf("renderTGS() error = %v", err)
	}
	if len(frames) != MaxVisionFrames {
		t.Fatalf("rendered frames = %d, want %d", len(frames), MaxVisionFrames)
	}
	if bytes.Equal(frames[0], frames[len(frames)-1]) {
		t.Fatal("sampled TGS frames are identical, want animation samples")
	}
}

func TestValidFilePathRejectsTraversal(t *testing.T) {
	for _, path := range []string{"../secret", "files/../../secret", "/absolute/file", "files\\secret"} {
		if validFilePath(path) {
			t.Errorf("validFilePath(%q) = true, want false", path)
		}
	}
	if !validFilePath("photos/file-name_1.png") || strings.Contains(escapeFilePath("photos/file-name_1.png"), "%2F") {
		t.Fatal("valid Telegram media path was rejected or escaped across path separators")
	}
}

func TestVideoDecodeArgsBoundInputDecoder(t *testing.T) {
	args := videoDecodeArgs()
	inputIndex := -1
	for index, argument := range args {
		if argument == "-i" {
			inputIndex = index
			break
		}
	}
	if inputIndex < 0 {
		t.Fatal("video decode args do not define an input")
	}
	for _, expected := range []struct {
		option string
		value  string
	}{
		{option: "-protocol_whitelist", value: "pipe"},
		{option: "-max_pixels", value: strconv.Itoa(MaxMediaPixels)},
		{option: "-threads", value: "0"},
	} {
		optionIndex := -1
		for index, argument := range args {
			if argument == expected.option {
				optionIndex = index
				break
			}
		}
		if optionIndex < 0 || optionIndex+1 >= inputIndex || args[optionIndex+1] != expected.value {
			t.Errorf("%s setting = %#v, want %q before input", expected.option, args, expected.value)
		}
	}
	if !strings.Contains(strings.Join(args, " "), "fps=4,") {
		t.Errorf("video filter = %#v, want 4fps candidate sampling", args)
	}
	for index, argument := range args {
		if argument == "-frames:v" && (index+1 >= len(args) || args[index+1] != strconv.Itoa(MaxVideoSampleFrames)) {
			t.Errorf("frame cap = %#v, want %d candidates", args, MaxVideoSampleFrames)
		}
	}
}

func TestValidateGIFBoundsLimitsTotalDecodedFramePixels(t *testing.T) {
	data := []byte("GIF89a")
	var screen [7]byte
	binary.LittleEndian.PutUint16(screen[0:2], 4000)
	binary.LittleEndian.PutUint16(screen[2:4], 4000)
	data = append(data, screen[:]...)
	appendFrame := func(width uint16, height uint16) {
		data = append(data, 0x2c)
		var descriptor [9]byte
		binary.LittleEndian.PutUint16(descriptor[4:6], width)
		binary.LittleEndian.PutUint16(descriptor[6:8], height)
		data = append(data, descriptor[:]...)
		data = append(data, 2, 1, 0x44, 0)
	}
	appendFrame(4000, 4000)
	appendFrame(1, 1)
	data = append(data, 0x3b)

	if err := validateGIFBounds(data); !errors.Is(err, ErrInvalidMedia) {
		t.Fatalf("validateGIFBounds() error = %v, want total decoded pixel budget rejection", err)
	}
}
