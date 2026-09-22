//go:build darwin

package pdfacceptance

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
)

const (
	sampleThumbnailWidth = 420
	sampleLabelHeight    = 28
	AcceptanceMaxSamples = 32
)

func BuildSamples(
	ctx context.Context,
	tool MuPDFTool,
	document AcceptanceDocument,
	pdfPath, outputPath string,
	requested []int,
	anchors []string,
) (SampleRecord, error) {
	if len(document.Pages) == 0 {
		return SampleRecord{}, errors.New("cannot render samples for an empty PDF")
	}
	absolute, err := filepath.Abs(outputPath)
	if err != nil {
		return SampleRecord{}, err
	}
	if _, err := os.Lstat(absolute); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return SampleRecord{}, fmt.Errorf("sample directory already exists: %s", absolute)
		}
		return SampleRecord{}, err
	}
	parent := filepath.Dir(absolute)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return SampleRecord{}, err
	}
	staging, err := os.MkdirTemp(parent, ".aoscx-pdf-samples-")
	if err != nil {
		return SampleRecord{}, err
	}
	if err := os.Chmod(staging, 0o700); err != nil {
		_ = os.RemoveAll(staging)
		return SampleRecord{}, err
	}
	stagingInfo, err := os.Lstat(staging)
	if err != nil {
		_ = os.RemoveAll(staging)
		return SampleRecord{}, err
	}
	success := false
	defer func() {
		if !success {
			if current, err := os.Lstat(staging); err == nil && os.SameFile(stagingInfo, current) {
				_ = os.RemoveAll(staging)
			}
			if current, err := os.Lstat(absolute); err == nil && os.SameFile(stagingInfo, current) {
				_ = os.RemoveAll(absolute)
			}
		}
	}()

	pageCount := len(document.Pages)
	selected := map[int]struct{}{
		1:                     {},
		pageCount:             {},
		max(1, pageCount/4):   {},
		max(1, pageCount/2):   {},
		max(1, pageCount*3/4): {},
	}
	for _, value := range requested {
		selected[max(1, min(pageCount, value))] = struct{}{}
	}
	anchorPages := map[string]*int{}
	for _, anchor := range anchors {
		target := NormalizeAcceptanceText(anchor)
		var found *int
		for index, page := range document.Pages {
			values := make([]string, len(page.Lines))
			for lineIndex, line := range page.Lines {
				values[lineIndex] = line.Text
			}
			if strings.Contains(NormalizeAcceptanceText(strings.Join(values, " ")), target) {
				page := index + 1
				found = &page
				selected[page] = struct{}{}
				break
			}
		}
		anchorPages[anchor] = found
	}
	if len(selected) > AcceptanceMaxSamples {
		return SampleRecord{}, fmt.Errorf("sample selection exceeds %d physical pages", AcceptanceMaxSamples)
	}
	physicalPages := sortedSet(selected)
	images := make([]image.Image, 0, len(physicalPages))
	for _, physicalPage := range physicalPages {
		name := fmt.Sprintf("physical-page-%05d.png", physicalPage)
		path := filepath.Join(staging, name)
		if err := tool.RenderPNG(ctx, pdfPath, path, physicalPage); err != nil {
			return SampleRecord{}, err
		}
		rendered, err := readBoundedPNG(path)
		if err != nil {
			return SampleRecord{}, err
		}
		images = append(images, labelledThumbnail(rendered, strings.ReplaceAll(strings.TrimSuffix(name, ".png"), "-", " ")))
		if err := syncRegularFile(path); err != nil {
			return SampleRecord{}, err
		}
	}
	montage := buildMontage(images)
	montagePath := filepath.Join(staging, "montage.png")
	if err := writeExclusivePNG(montagePath, montage); err != nil {
		return SampleRecord{}, err
	}
	if err := syncDirectory(staging); err != nil {
		return SampleRecord{}, err
	}
	if _, err := os.Lstat(absolute); !errors.Is(err, os.ErrNotExist) {
		return SampleRecord{}, fmt.Errorf("sample directory destination appeared before publication: %s", absolute)
	}
	if err := os.Rename(staging, absolute); err != nil {
		return SampleRecord{}, err
	}
	if err := syncDirectory(parent); err != nil {
		return SampleRecord{}, err
	}
	success = true
	return SampleRecord{
		AnchorPages: anchorPages, Montage: filepath.Join(absolute, "montage.png"),
		PhysicalPages: physicalPages,
	}, nil
}

func readBoundedPNG(path string) (image.Image, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() ||
		info.Size() < 1 || info.Size() > MuPDFSampleMaxBytes {
		return nil, fmt.Errorf("sample PNG is not a bounded nonsymlink regular file: %s", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		_ = file.Close()
		return nil, fmt.Errorf("sample PNG identity changed while opening: %s", path)
	}
	config, err := png.DecodeConfig(io.LimitReader(file, MuPDFSampleMaxBytes+1))
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if config.Width < 1 || config.Height < 1 || config.Width > 20_000 || config.Height > 20_000 ||
		int64(config.Width)*int64(config.Height) > 100_000_000 {
		_ = file.Close()
		return nil, errors.New("sample PNG dimensions exceed acceptance bounds")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		return nil, err
	}
	rendered, decodeErr := png.Decode(io.LimitReader(file, MuPDFSampleMaxBytes+1))
	closeErr := file.Close()
	if err := errors.Join(decodeErr, closeErr); err != nil {
		return nil, err
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, current) {
		return nil, fmt.Errorf("sample PNG identity changed while reading: %s", path)
	}
	return rendered, nil
}

func labelledThumbnail(source image.Image, label string) image.Image {
	bounds := source.Bounds()
	height := int(math.Ceil(float64(bounds.Dy()) * sampleThumbnailWidth / float64(bounds.Dx())))
	thumbnail := image.NewRGBA(image.Rect(0, 0, sampleThumbnailWidth, height+sampleLabelHeight))
	draw.Draw(thumbnail, thumbnail.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	for y := 0; y < height; y++ {
		sourceY := bounds.Min.Y + min(bounds.Dy()-1, y*bounds.Dy()/height)
		for x := 0; x < sampleThumbnailWidth; x++ {
			sourceX := bounds.Min.X + min(bounds.Dx()-1, x*bounds.Dx()/sampleThumbnailWidth)
			thumbnail.Set(x, y+sampleLabelHeight, source.At(sourceX, sourceY))
		}
	}
	drawBitmapText(thumbnail, 8, 7, label)
	return thumbnail
}

func buildMontage(images []image.Image) image.Image {
	if len(images) == 0 {
		return image.NewRGBA(image.Rect(0, 0, 1, 1))
	}
	columns := min(3, len(images))
	rows := int(math.Ceil(float64(len(images)) / float64(columns)))
	cellHeight := 0
	for _, value := range images {
		cellHeight = max(cellHeight, value.Bounds().Dy())
	}
	result := image.NewRGBA(image.Rect(0, 0, columns*sampleThumbnailWidth, rows*cellHeight))
	draw.Draw(result, result.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	for index, value := range images {
		target := image.Rect(
			(index%columns)*sampleThumbnailWidth,
			(index/columns)*cellHeight,
			(index%columns)*sampleThumbnailWidth+value.Bounds().Dx(),
			(index/columns)*cellHeight+value.Bounds().Dy(),
		)
		draw.Draw(result, target, value, value.Bounds().Min, draw.Src)
	}
	return result
}

func writeExclusivePNG(path string, value image.Image) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	encodeErr := png.Encode(file, value)
	err = errors.Join(encodeErr, file.Sync(), file.Close())
	if err != nil {
		_ = os.Remove(path)
	}
	return err
}

func syncRegularFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(file.Sync(), file.Close())
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

func drawBitmapText(target draw.Image, x, y int, value string) {
	for _, character := range strings.ToLower(value) {
		rows, ok := bitmapFont[character]
		if !ok {
			rows = bitmapFont['?']
		}
		for row, bits := range rows {
			for column := 0; column < 5; column++ {
				if bits&(1<<uint(4-column)) != 0 {
					target.Set(x+column, y+row, color.Black)
				}
			}
		}
		x += 6
	}
}

var bitmapFont = map[rune][7]byte{
	' ': {},
	'?': {0x0e, 0x11, 0x01, 0x02, 0x04, 0x00, 0x04},
	'a': {0x00, 0x0e, 0x01, 0x0f, 0x11, 0x13, 0x0d},
	'c': {0x00, 0x0e, 0x11, 0x10, 0x10, 0x11, 0x0e},
	'e': {0x00, 0x0e, 0x11, 0x1f, 0x10, 0x11, 0x0e},
	'g': {0x00, 0x0f, 0x11, 0x0f, 0x01, 0x11, 0x0e},
	'h': {0x10, 0x10, 0x16, 0x19, 0x11, 0x11, 0x11},
	'i': {0x04, 0x00, 0x0c, 0x04, 0x04, 0x04, 0x0e},
	'l': {0x0c, 0x04, 0x04, 0x04, 0x04, 0x04, 0x0e},
	'p': {0x00, 0x1e, 0x11, 0x1e, 0x10, 0x10, 0x10},
	's': {0x00, 0x0f, 0x10, 0x0e, 0x01, 0x11, 0x0e},
	'y': {0x00, 0x11, 0x11, 0x0f, 0x01, 0x11, 0x0e},
	'0': {0x0e, 0x11, 0x13, 0x15, 0x19, 0x11, 0x0e},
	'1': {0x04, 0x0c, 0x14, 0x04, 0x04, 0x04, 0x1f},
	'2': {0x0e, 0x11, 0x01, 0x02, 0x04, 0x08, 0x1f},
	'3': {0x1e, 0x01, 0x01, 0x0e, 0x01, 0x01, 0x1e},
	'4': {0x02, 0x06, 0x0a, 0x12, 0x1f, 0x02, 0x02},
	'5': {0x1f, 0x10, 0x10, 0x1e, 0x01, 0x01, 0x1e},
	'6': {0x0e, 0x10, 0x10, 0x1e, 0x11, 0x11, 0x0e},
	'7': {0x1f, 0x01, 0x02, 0x04, 0x08, 0x08, 0x08},
	'8': {0x0e, 0x11, 0x11, 0x0e, 0x11, 0x11, 0x0e},
	'9': {0x0e, 0x11, 0x11, 0x0f, 0x01, 0x01, 0x0e},
}
