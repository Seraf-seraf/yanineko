package telegram

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	stdraw "image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"net/http"
	"net/url"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/Seraf-seraf/yanineko/internal/domain"
	"github.com/xo/lottie"
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const MaxTelegramFileBytes int64 = 20 << 20
const MaxMediaPixels = 16_000_000
const MaxAnimatedFrames = 60
const MaxVisionFrames = 4
const MaxVisionDimension = 1024
const MaxVisionRequestBytes int64 = 10 << 20
const MaxVisionFrameBytes int64 = MaxVisionRequestBytes / MaxVisionFrames
const MaxTGSJSONBytes int64 = 2 << 20
const MaxFrameStreamBytes int64 = 8 << 20
const MediaFetchTimeout = 4 * time.Second
const MaxStickerVideoDuration = 3 * time.Second
const MaxTGSAnimationDuration = 3100 * time.Millisecond

var ErrInvalidMedia = errors.New("invalid Telegram media")

type telegramFile struct {
	Path     string `json:"file_path"`
	FileSize int64  `json:"file_size"`
}

type mediaReference struct {
	fileID    string
	fileSize  int64
	kind      string
	thumbnail *PhotoSize
}

// LoadMedia fetches supported media from the current message and, when present,
// the message it replies to. Downloaded bytes remain in memory only.
func (c *Client) LoadMedia(ctx context.Context, update Update) ([]domain.Image, error) {
	if c == nil || update.Message == nil {
		return nil, ErrInvalidTelegramRequest
	}
	message := update.Message
	refs := mediaReferences(message)
	if len(refs) == 0 && message.ReplyToMessage != nil {
		refs = mediaReferences(message.ReplyToMessage)
	}
	if len(refs) == 0 {
		return nil, nil
	}
	if len(refs) > 2 {
		refs = refs[:2]
	}
	images := make([]domain.Image, 0, MaxVisionFrames)
	for _, ref := range refs {
		data, err := c.downloadFile(ctx, ref.fileID, ref.fileSize)
		if err != nil {
			return nil, err
		}
		frames, err := decodeMediaFrames(ctx, ref.kind, data)
		if err != nil && ref.thumbnail != nil {
			thumb, thumbErr := c.downloadFile(ctx, ref.thumbnail.FileID, ref.thumbnail.FileSize)
			if thumbErr == nil {
				frames, err = decodeMediaFrames(ctx, "image", thumb)
			}
		}
		if err != nil {
			return nil, err
		}
		for _, frame := range frames {
			images = append(images, domain.Image{Data: frame, MIMEType: "image/png"})
			if len(images) == MaxVisionFrames {
				return images, nil
			}
		}
	}
	return images, nil
}

func mediaReferences(message *Message) []mediaReference {
	if message == nil {
		return nil
	}
	if len(message.Photo) > 0 {
		best := message.Photo[0]
		for _, photo := range message.Photo[1:] {
			if int64(photo.Width)*int64(photo.Height) > int64(best.Width)*int64(best.Height) {
				best = photo
			}
		}
		return []mediaReference{{fileID: best.FileID, fileSize: best.FileSize, kind: "image"}}
	}
	if message.Animation != nil {
		return []mediaReference{{fileID: message.Animation.FileID, fileSize: message.Animation.FileSize, kind: "animation", thumbnail: message.Animation.Thumbnail}}
	}
	if isImageDocument(message.Document) {
		return []mediaReference{{fileID: message.Document.FileID, fileSize: message.Document.FileSize, kind: "image"}}
	}
	if message.Sticker != nil {
		kind := "image"
		if message.Sticker.IsAnimated {
			kind = "tgs"
		} else if message.Sticker.IsVideo {
			kind = "video"
		}
		return []mediaReference{{fileID: message.Sticker.FileID, fileSize: message.Sticker.FileSize, kind: kind, thumbnail: message.Sticker.Thumbnail}}
	}
	return nil
}

func (c *Client) downloadFile(ctx context.Context, fileID string, expectedSize int64) ([]byte, error) {
	if !validFileID(fileID) || expectedSize < 0 || expectedSize > MaxTelegramFileBytes {
		return nil, ErrInvalidMedia
	}
	var file telegramFile
	if err := c.callWithTimeout(ctx, "getFile", map[string]string{"file_id": fileID}, &file, MediaFetchTimeout); err != nil {
		return nil, err
	}
	if !validFilePath(file.Path) || file.FileSize < 0 || file.FileSize > MaxTelegramFileBytes {
		return nil, ErrInvalidMedia
	}
	requestContext, cancel := context.WithTimeout(ctx, MediaFetchTimeout)
	defer cancel()
	urlPath := c.baseURL + "/file/bot" + c.token + "/" + escapeFilePath(file.Path)
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, urlPath, nil)
	if err != nil {
		return nil, ErrInvalidTelegramRequest
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		if requestContext.Err() != nil {
			return nil, requestContext.Err()
		}
		return nil, ErrTelegramRequest
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, ErrTelegramAPI
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, MaxTelegramFileBytes+1))
	if err != nil {
		if requestContext.Err() != nil {
			return nil, requestContext.Err()
		}
		return nil, ErrInvalidMedia
	}
	if int64(len(data)) > MaxTelegramFileBytes || len(data) == 0 {
		return nil, ErrInvalidMedia
	}
	return data, nil
}

func decodeMediaFrames(ctx context.Context, kind string, data []byte) ([][]byte, error) {
	if len(data) == 0 || int64(len(data)) > MaxTelegramFileBytes {
		return nil, ErrInvalidMedia
	}
	if kind == "tgs" {
		return renderTGS(ctx, data)
	}
	if kind == "video" || kind == "animation" && !bytes.HasPrefix(data, []byte("GIF8")) {
		return decodeVideoFrames(ctx, data)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > MaxMediaPixels {
		return nil, ErrInvalidMedia
	}
	if format == "gif" {
		return decodeGIFFrames(data)
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, ErrInvalidMedia
	}
	return encodeImageFrames([]image.Image{decoded})
}

func decodeGIFFrames(data []byte) ([][]byte, error) {
	if err := validateGIFBounds(data); err != nil {
		return nil, err
	}
	decoded, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil || len(decoded.Image) == 0 || len(decoded.Image) > MaxAnimatedFrames {
		return nil, ErrInvalidMedia
	}
	if decoded.Config.Width <= 0 || decoded.Config.Height <= 0 || int64(decoded.Config.Width)*int64(decoded.Config.Height) > MaxMediaPixels {
		return nil, ErrInvalidMedia
	}
	selected := selectedFrameIndices(len(decoded.Image))
	selectedIndex := 0
	frames := make([][]byte, 0, len(selected))
	canvasBounds := image.Rect(0, 0, decoded.Config.Width, decoded.Config.Height)
	canvas := image.NewRGBA(canvasBounds)
	background := gifBackground(decoded)
	stdraw.Draw(canvas, canvasBounds, image.NewUniform(background), image.Point{}, stdraw.Src)
	var previousBounds image.Rectangle
	var previousRestore *image.RGBA
	var previousDisposal byte
	for index, frame := range decoded.Image {
		if index > 0 {
			switch previousDisposal {
			case gif.DisposalBackground:
				stdraw.Draw(canvas, previousBounds, image.NewUniform(background), image.Point{}, stdraw.Src)
			case gif.DisposalPrevious:
				if previousRestore == nil {
					return nil, ErrInvalidMedia
				}
				stdraw.Draw(canvas, previousBounds, previousRestore, previousBounds.Min, stdraw.Src)
			}
		}
		frameBounds := frame.Bounds()
		if frameBounds.Empty() || !frameBounds.In(canvasBounds) {
			return nil, ErrInvalidMedia
		}
		disposal := byte(0)
		if index < len(decoded.Disposal) {
			disposal = decoded.Disposal[index]
		}
		var restore *image.RGBA
		if disposal == gif.DisposalPrevious {
			restore = image.NewRGBA(frameBounds)
			stdraw.Draw(restore, frameBounds, canvas, frameBounds.Min, stdraw.Src)
		}
		stdraw.Draw(canvas, frameBounds, frame, frameBounds.Min, stdraw.Over)
		if selectedIndex < len(selected) && index == selected[selectedIndex] {
			encoded, err := encodeVisionFrame(canvas)
			if err != nil {
				return nil, err
			}
			frames = append(frames, encoded)
			selectedIndex++
		}
		previousBounds = frameBounds
		previousDisposal = disposal
		previousRestore = restore
	}
	return frames, nil
}

func gifBackground(decoded *gif.GIF) color.Color {
	palette, ok := decoded.Config.ColorModel.(color.Palette)
	if !ok || int(decoded.BackgroundIndex) >= len(palette) {
		return color.Transparent
	}
	return palette[decoded.BackgroundIndex]
}

func validateGIFBounds(data []byte) error {
	if len(data) < 13 || (!bytes.HasPrefix(data, []byte("GIF87a")) && !bytes.HasPrefix(data, []byte("GIF89a"))) {
		return ErrInvalidMedia
	}
	width := int(data[6]) | int(data[7])<<8
	height := int(data[8]) | int(data[9])<<8
	if width <= 0 || height <= 0 || width*height > MaxMediaPixels {
		return ErrInvalidMedia
	}
	offset := 13
	packed := data[10]
	if packed&0x80 != 0 {
		offset += 3 * (1 << ((packed & 7) + 1))
	}
	frameCount := 0
	decodedFramePixels := int64(0)
	for offset < len(data) {
		marker := data[offset]
		offset++
		switch marker {
		case 0x3b:
			if frameCount == 0 {
				return ErrInvalidMedia
			}
			return nil
		case 0x21:
			if offset >= len(data) {
				return ErrInvalidMedia
			}
			offset++
			var ok bool
			offset, ok = skipGIFSubBlocks(data, offset)
			if !ok {
				return ErrInvalidMedia
			}
		case 0x2c:
			if len(data)-offset < 9 {
				return ErrInvalidMedia
			}
			left := int(data[offset]) | int(data[offset+1])<<8
			top := int(data[offset+2]) | int(data[offset+3])<<8
			frameWidth := int(data[offset+4]) | int(data[offset+5])<<8
			frameHeight := int(data[offset+6]) | int(data[offset+7])<<8
			framePixels := int64(frameWidth) * int64(frameHeight)
			if frameWidth <= 0 || frameHeight <= 0 || frameWidth*frameHeight > MaxMediaPixels ||
				left+frameWidth > width || top+frameHeight > height ||
				frameCount >= MaxAnimatedFrames || decodedFramePixels+framePixels > int64(MaxMediaPixels) {
				return ErrInvalidMedia
			}
			frameCount++
			decodedFramePixels += framePixels
			localPacked := data[offset+8]
			offset += 9
			if localPacked&0x80 != 0 {
				offset += 3 * (1 << ((localPacked & 7) + 1))
			}
			if offset >= len(data) {
				return ErrInvalidMedia
			}
			offset++
			var ok bool
			offset, ok = skipGIFSubBlocks(data, offset)
			if !ok {
				return ErrInvalidMedia
			}
		default:
			return ErrInvalidMedia
		}
	}
	return ErrInvalidMedia
}

func skipGIFSubBlocks(data []byte, offset int) (int, bool) {
	for offset < len(data) {
		blockSize := int(data[offset])
		offset++
		if blockSize == 0 {
			return offset, true
		}
		if len(data)-offset < blockSize {
			return 0, false
		}
		offset += blockSize
	}
	return 0, false
}

func selectFrames[T any](frames []T) []T {
	indices := selectedFrameIndices(len(frames))
	selected := make([]T, 0, len(indices))
	for _, index := range indices {
		selected = append(selected, frames[index])
	}
	return selected
}

func selectedFrameIndices(count int) []int {
	if count <= 0 {
		return nil
	}
	selectedCount := min(count, MaxVisionFrames)
	indices := make([]int, 0, selectedCount)
	for index := 0; index < selectedCount; index++ {
		if selectedCount == 1 {
			indices = append(indices, 0)
			continue
		}
		indices = append(indices, index*(count-1)/(selectedCount-1))
	}
	return indices
}

func renderTGS(ctx context.Context, compressed []byte) ([][]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, mediaDecodeError(ctx)
	}
	jsonData, err := io.ReadAll(io.LimitReader(reader, MaxTGSJSONBytes+1))
	closeErr := reader.Close()
	if err != nil || closeErr != nil || int64(len(jsonData)) > MaxTGSJSONBytes || !json.Valid(jsonData) {
		return nil, mediaDecodeError(ctx)
	}
	renderer, err := lottie.New(ctx)
	if err != nil {
		return nil, mediaDecodeError(ctx)
	}
	defer renderer.Close(context.Background())
	animation, err := renderer.Load(ctx, jsonData, lottie.Options{Width: 512, Height: 512, Quality: 60})
	if err != nil {
		return nil, mediaDecodeError(ctx)
	}
	defer animation.Close(context.Background())
	if animation.TotalFrames() <= 0 || animation.TotalFrames() > 180 || animation.Duration() <= 0 ||
		time.Duration(float64(animation.Duration())*float64(time.Second)) > MaxTGSAnimationDuration {
		return nil, ErrInvalidMedia
	}
	frames, err := animation.FramesAt(ctx, MaxVisionFrames)
	if err != nil || len(frames) == 0 {
		return nil, mediaDecodeError(ctx)
	}
	return encodeImageFrames(frames)
}

func mediaDecodeError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return ErrInvalidMedia
}

func decodeVideoFrames(ctx context.Context, data []byte) ([][]byte, error) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil, ErrInvalidMedia
	}
	command := exec.CommandContext(ctx, ffmpeg, videoDecodeArgs()...)
	command.Stdin = bytes.NewReader(data)
	var output boundedBuffer
	output.limit = MaxFrameStreamBytes
	command.Stdout = &output
	command.Stderr = io.Discard
	if err := command.Run(); err != nil || len(output.data) == 0 {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrInvalidMedia
	}
	parts := splitJPEGFrames(output.data)
	if len(parts) == 0 || len(parts) > MaxVisionFrames {
		return nil, ErrInvalidMedia
	}
	frames := make([]image.Image, 0, len(parts))
	for _, part := range parts {
		frame, err := jpeg.Decode(bytes.NewReader(part))
		if err != nil || frame.Bounds().Dx()*frame.Bounds().Dy() > MaxMediaPixels {
			return nil, ErrInvalidMedia
		}
		frames = append(frames, frame)
	}
	return encodeImageFrames(frames)
}

func videoDecodeArgs() []string {
	return []string{
		"-nostdin", "-v", "error",
		"-protocol_whitelist", "pipe",
		"-max_pixels", strconv.Itoa(MaxMediaPixels),
		"-threads", "0",
		"-t", strconv.FormatFloat(MaxStickerVideoDuration.Seconds(), 'f', -1, 64),
		"-i", "pipe:0",
		"-vf", "fps=1.333,scale='min(512,iw)':'min(512,ih)':force_original_aspect_ratio=decrease",
		"-frames:v", strconv.Itoa(MaxVisionFrames),
		"-f", "image2pipe", "-vcodec", "mjpeg", "pipe:1",
	}
}

type boundedBuffer struct {
	data  []byte
	limit int64
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if int64(len(b.data)+len(p)) > b.limit {
		return 0, ErrInvalidMedia
	}
	b.data = append(b.data, p...)
	return len(p), nil
}

func splitJPEGFrames(data []byte) [][]byte {
	frames := make([][]byte, 0, MaxVisionFrames)
	start := -1
	for i := 0; i+1 < len(data); i++ {
		if data[i] == 0xff && data[i+1] == 0xd8 {
			start = i
			i++
			continue
		}
		if start >= 0 && data[i] == 0xff && data[i+1] == 0xd9 {
			frames = append(frames, data[start:i+2])
			start = -1
			if len(frames) == MaxVisionFrames {
				break
			}
			i++
		}
	}
	return frames
}

func encodeImageFrames(frames []image.Image) ([][]byte, error) {
	frames = selectFrames(frames)
	encoded := make([][]byte, 0, len(frames))
	for _, frame := range frames {
		if frame == nil || frame.Bounds().Empty() || int64(frame.Bounds().Dx())*int64(frame.Bounds().Dy()) > MaxMediaPixels {
			return nil, ErrInvalidMedia
		}
		data, err := encodeVisionFrame(frame)
		if err != nil {
			return nil, ErrInvalidMedia
		}
		encoded = append(encoded, data)
	}
	return encoded, nil
}

func encodeVisionFrame(frame image.Image) ([]byte, error) {
	width, height := frame.Bounds().Dx(), frame.Bounds().Dy()
	scale := math.Min(1, math.Min(float64(MaxVisionDimension)/float64(width), float64(MaxVisionDimension)/float64(height)))
	width = max(1, int(float64(width)*scale))
	height = max(1, int(float64(height)*scale))
	for {
		destination := image.NewNRGBA(image.Rect(0, 0, width, height))
		xdraw.CatmullRom.Scale(destination, destination.Bounds(), frame, frame.Bounds(), xdraw.Over, nil)
		var buffer bytes.Buffer
		if err := png.Encode(&buffer, destination); err != nil {
			return nil, ErrInvalidMedia
		}
		if int64(buffer.Len()) <= MaxVisionFrameBytes {
			return buffer.Bytes(), nil
		}
		if width == 1 && height == 1 {
			return nil, ErrInvalidMedia
		}
		width = max(1, width*3/4)
		height = max(1, height*3/4)
	}
}

func validFileID(fileID string) bool {
	if fileID == "" || len(fileID) > 512 || strings.TrimSpace(fileID) != fileID {
		return false
	}
	for _, character := range fileID {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_' || character == '-') {
			return false
		}
	}
	return true
}

func validFilePath(filePath string) bool {
	if filePath == "" || len(filePath) > 1024 || strings.Contains(filePath, "\\") || path.IsAbs(filePath) {
		return false
	}
	for _, segment := range strings.Split(filePath, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
		for _, character := range segment {
			if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || strings.ContainsRune("._-", character)) {
				return false
			}
		}
	}
	return true
}

func escapeFilePath(filePath string) string {
	parts := strings.Split(filePath, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}
