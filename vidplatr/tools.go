package vidplatr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// GetDurationMs returns the media duration in milliseconds.
//
// This is intended for API-layer usage since markers use int64 millisecond
// timestamps.
func GetDurationMs(ctx context.Context, filePath string) (int64, error) {
	if strings.TrimSpace(filePath) == "" {
		return 0, errors.New("filePath is empty")
	}
	mediaDurationMs, err := probeDurationMs(ctx, filePath)
	if err != nil {
		return 0, err
	}
	return mediaDurationMs, nil
}

// ================= //
//    Delete Video   //
// ================= //

// DeleteFile removes the video file from disk.
//
// This is idempotent: if the file does not exist, it returns nil.
func DeleteFile(filePath string) error {
	if strings.TrimSpace(filePath) == "" {
		return errors.New("filePath is empty")
	}
	if err := os.Remove(filePath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return nil
}

// ================= //
//     Copy Video    //
// ================= //

// CopyFile remuxes (copies) the first video stream and optional audio stream
// into a new container without re-encoding.
//
// This is basically: input -> output, with `-c copy`.
func CopyFile(ctx context.Context, inputPath, outputPath string) error {
	if strings.TrimSpace(inputPath) == "" {
		return errors.New("inputPath is empty")
	}
	if strings.TrimSpace(outputPath) == "" {
		return errors.New("outputPath is empty")
	}
	if err := ensureParentDir(outputPath); err != nil {
		return err
	}

	ffmpegArgs := []string{
		"-y",
		"-hide_banner",
		"-loglevel", "error",
		"-i", inputPath,
		"-map", "0:v:0",
		"-map", "0:a?",
		"-c", "copy",
	}
	ffmpegArgs = append(ffmpegArgs, movflags(outputPath)...)
	ffmpegArgs = append(ffmpegArgs, outputPath)

	_, err := runFFmpeg(ctx, ffmpegArgs...)
	return err
}

// ================= //
//
//	Copy Video Clip
//
// between 2 markers //
// ================= //

// CopySection copies a time range out of a video.
//
// startMs/endMs are milliseconds (matching models.Marker.TimeStamp).
//
// It first tries a fast stream-copy cut (`-c copy`). Stream copy is quick but the
// cut may only be exact on keyframes. If that fails, it falls back to re-encoding
// for accuracy.
func CopySection(ctx context.Context, inputPath, outputPath string, startMs, endMs int64) error {
	if strings.TrimSpace(inputPath) == "" {
		return errors.New("inputPath is empty")
	}
	if strings.TrimSpace(outputPath) == "" {
		return errors.New("outputPath is empty")
	}
	if startMs < 0 {
		return errors.New("startMs must be >= 0")
	}
	if endMs <= startMs {
		return errors.New("endMs must be > startMs")
	}
	if err := ensureParentDir(outputPath); err != nil {
		return err
	}

	startTimestamp := formatTimestampMs(startMs)
	clipDuration := formatTimestampMs(endMs - startMs)

	// Attempt 1: fast stream copy.
	ffmpegCopyArgs := []string{
		"-y",
		"-hide_banner",
		"-loglevel", "error",
		"-ss", startTimestamp,
		"-t", clipDuration,
		"-i", inputPath,
		"-map", "0:v:0",
		"-map", "0:a?",
		"-c", "copy",
		"-avoid_negative_ts", "make_zero",
	}
	ffmpegCopyArgs = append(ffmpegCopyArgs, movflags(outputPath)...)
	ffmpegCopyArgs = append(ffmpegCopyArgs, outputPath)

	copyErrOutput, copyErr := runFFmpeg(ctx, ffmpegCopyArgs...)
	if copyErr == nil {
		return nil
	}

	// Attempt 2: re-encode fallback (accurate cuts).
	ffmpegReencodeArgs := []string{
		"-y",
		"-hide_banner",
		"-loglevel", "error",
		"-ss", startTimestamp,
		"-t", clipDuration,
		"-i", inputPath,
		"-map", "0:v:0",
		"-map", "0:a?",
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-crf", "20",
		"-pix_fmt", "yuv420p",
		"-c:a", "aac",
		"-b:a", "192k",
	}
	ffmpegReencodeArgs = append(ffmpegReencodeArgs, movflags(outputPath)...)
	ffmpegReencodeArgs = append(ffmpegReencodeArgs, outputPath)

	reencodeErrOutput, reencodeErr := runFFmpeg(ctx, ffmpegReencodeArgs...)
	if reencodeErr != nil {
		return errors.New(strings.TrimSpace("stream-copy failed: " + copyErrOutput + "\nre-encode failed: " + reencodeErrOutput))
	}
	return nil
}

// ================= //
//
// 	Delete Video Clip
//
// between 2 markers //
// ================= //

// DeleteSection deletes a time range out of a video.
//
// startMs/endMs are milliseconds (matching models.Marker.TimeStamp).
//
// It produces a new file that contains everything EXCEPT the deleted range.
func DeleteSection(ctx context.Context, inputPath, outputPath string, startMs, endMs int64) error {
	if strings.TrimSpace(inputPath) == "" {
		return errors.New("inputPath is empty")
	}
	if strings.TrimSpace(outputPath) == "" {
		return errors.New("outputPath is empty")
	}
	if startMs < 0 {
		return errors.New("startMs must be >= 0")
	}
	if endMs <= startMs {
		return errors.New("endMs must be > startMs")
	}
	if err := ensureParentDir(outputPath); err != nil {
		return err
	}

	durationMs, err := probeDurationMs(ctx, inputPath)
	if err != nil {
		return err
	}
	if startMs >= durationMs {
		return errors.New("startMs must be < duration")
	}
	if endMs > durationMs {
		endMs = durationMs
	}
	if startMs == 0 && endMs >= durationMs {
		if inputPath == outputPath {
			return DeleteFile(outputPath)
		}
		return errors.New("delete range removes entire video")
	}

	dir := filepath.Dir(outputPath)
	ext := filepath.Ext(outputPath)

	mkTemp := func(prefix string) (string, error) {
		f, err := os.CreateTemp(dir, prefix+"-*"+ext)
		if err != nil {
			return "", err
		}
		name := f.Name()
		_ = f.Close()
		return name, nil
	}

	var partA string
	if startMs > 0 {
		partA, err = mkTemp("mvedit-partA")
		if err != nil {
			return err
		}
		defer os.Remove(partA)
		if err := CopySection(ctx, inputPath, partA, 0, startMs); err != nil {
			return err
		}
	}

	var partB string
	if endMs < durationMs {
		partB, err = mkTemp("mvedit-partB")
		if err != nil {
			return err
		}
		defer os.Remove(partB)
		if err := CopySection(ctx, inputPath, partB, endMs, durationMs); err != nil {
			return err
		}
	}

	// If we only have one side, it becomes the final file.
	if partA == "" && partB == "" {
		return errors.New("nothing to keep after delete")
	}
	if partA == "" {
		return os.Rename(partB, outputPath)
	}
	if partB == "" {
		return os.Rename(partA, outputPath)
	}

	listFile, listCleanup, err := createConcatListFile(dir, []string{partA, partB})
	if err != nil {
		return err
	}
	defer listCleanup()

	outTmp, err := tempLike(outputPath, "mvedit-joined")
	if err != nil {
		return err
	}
	defer os.Remove(outTmp)

	if err := concatListCopyOrReencode(ctx, listFile, outTmp); err != nil {
		return err
	}

	return os.Rename(outTmp, outputPath)
}

// ================= //
//    Append Video   //
// ================= //

// AppendFile appends a video to another video.
//
// Returns the timestamp (ms) of the new final length.
func AppendFile(ctx context.Context, inputPath, appendPath, outputPath string) (int64, error) {
	if strings.TrimSpace(inputPath) == "" {
		return 0, errors.New("inputPath is empty")
	}
	if strings.TrimSpace(appendPath) == "" {
		return 0, errors.New("appendPath is empty")
	}
	if strings.TrimSpace(outputPath) == "" {
		return 0, errors.New("outputPath is empty")
	}
	if err := ensureParentDir(outputPath); err != nil {
		return 0, err
	}

	outPath, finalize, cleanup, err := prepareInPlaceOutput(inputPath, outputPath, "mvedit-append")
	if err != nil {
		return 0, err
	}
	defer cleanup()

	listPath, listCleanup, err := createConcatListFile(filepath.Dir(outPath), []string{inputPath, appendPath})
	if err != nil {
		return 0, err
	}
	defer listCleanup()

	if err := concatListCopyOrReencode(ctx, listPath, outPath); err != nil {
		return 0, err
	}
	if err := finalize(); err != nil {
		return 0, err
	}

	newDurationMs, err := probeDurationMs(ctx, outputPath)
	if err != nil {
		return 0, err
	}
	return newDurationMs, nil
}

// ================================ //
//
// 	Append Video Clip after marker
//
// ================================ //

// AppendSection appends a video clip after the passed in timestamp.
//
// Anything originally after insertMs gets pushed back by the duration of appendPath.
//
// Returns the timestamp (ms) where the new appended section ends.
func AppendSection(ctx context.Context, inputPath, appendPath, outputPath string, insertMs int64) (int64, error) {
	if strings.TrimSpace(inputPath) == "" {
		return 0, errors.New("inputPath is empty")
	}
	if strings.TrimSpace(appendPath) == "" {
		return 0, errors.New("appendPath is empty")
	}
	if strings.TrimSpace(outputPath) == "" {
		return 0, errors.New("outputPath is empty")
	}
	if insertMs < 0 {
		return 0, errors.New("insertMs must be >= 0")
	}
	if err := ensureParentDir(outputPath); err != nil {
		return 0, err
	}

	inputDurationMs, err := probeDurationMs(ctx, inputPath)
	if err != nil {
		return 0, err
	}
	if insertMs > inputDurationMs {
		return 0, errors.New("insertMs must be <= duration")
	}

	appendDurationMs, err := probeDurationMs(ctx, appendPath)
	if err != nil {
		return 0, err
	}

	// Prepend shortcut.
	if insertMs == 0 {
		if _, err := AppendFile(ctx, appendPath, inputPath, outputPath); err != nil {
			return 0, err
		}
		return appendDurationMs, nil
	}

	// Append shortcut.
	if insertMs == inputDurationMs {
		newFinalMs, err := AppendFile(ctx, inputPath, appendPath, outputPath)
		if err != nil {
			return 0, err
		}
		return newFinalMs, nil
	}

	outPath := outputPath
	finalize := func() error { return nil }
	cleanupTmp := func() {}
	if outputPath == inputPath {
		outPath, finalize, cleanupTmp, err = prepareInPlaceOutput(inputPath, outputPath, "mvedit-insert")
		if err != nil {
			return 0, err
		}
		defer cleanupTmp()
	}

	dir := filepath.Dir(outPath)

	partA, err := tempLike(outPath, "mvedit-partA")
	if err != nil {
		return 0, err
	}
	defer os.Remove(partA)

	partB, err := tempLike(outPath, "mvedit-partB")
	if err != nil {
		return 0, err
	}
	defer os.Remove(partB)

	if err := CopySection(ctx, inputPath, partA, 0, insertMs); err != nil {
		return 0, err
	}
	if err := CopySection(ctx, inputPath, partB, insertMs, inputDurationMs); err != nil {
		return 0, err
	}

	partADurationMs, err := probeDurationMs(ctx, partA)
	if err != nil {
		return 0, err
	}
	newSectionEndsMs := partADurationMs + appendDurationMs

	listPath, listCleanup, err := createConcatListFile(dir, []string{partA, appendPath, partB})
	if err != nil {
		return 0, err
	}
	defer listCleanup()

	if err := concatListCopyOrReencode(ctx, listPath, outPath); err != nil {
		return 0, err
	}

	if err := finalize(); err != nil {
		return 0, err
	}

	return newSectionEndsMs, nil
}

// ================= //
//
// 	Replace Video Clip
//
// between 2 markers //
// ================= //

// ReplaceSection replaces a time range out of a video with another video clip.
//
// startMs/endMs are milliseconds (matching models.Marker.TimeStamp).
//
// It produces a new file where the section between startMs and endMs is replaced
// by replacePath.
//
// Returns the new timestamp (ms) that corresponds to the original endMs.
func ReplaceSection(ctx context.Context, inputPath, replacePath, outputPath string, startMs, endMs int64) (int64, error) {
	if strings.TrimSpace(inputPath) == "" {
		return 0, errors.New("inputPath is empty")
	}
	if strings.TrimSpace(replacePath) == "" {
		return 0, errors.New("replacePath is empty")
	}
	if strings.TrimSpace(outputPath) == "" {
		return 0, errors.New("outputPath is empty")
	}
	if startMs < 0 {
		return 0, errors.New("startMs must be >= 0")
	}
	if endMs <= startMs {
		return 0, errors.New("endMs must be > startMs")
	}
	if err := ensureParentDir(outputPath); err != nil {
		return 0, err
	}

	inputDurationMs, err := probeDurationMs(ctx, inputPath)
	if err != nil {
		return 0, err
	}
	if startMs >= inputDurationMs {
		return 0, errors.New("startMs must be < duration")
	}
	if endMs > inputDurationMs {
		endMs = inputDurationMs
	}

	replaceDurationMs, err := probeDurationMs(ctx, replacePath)
	if err != nil {
		return 0, err
	}

	// Whole-file replace.
	if startMs == 0 && endMs >= inputDurationMs {
		if err := CopyFile(ctx, replacePath, outputPath); err != nil {
			return 0, err
		}
		return replaceDurationMs, nil
	}

	outPath := outputPath
	finalize := func() error { return nil }
	cleanupTmp := func() {}
	if outputPath == inputPath {
		outPath, finalize, cleanupTmp, err = prepareInPlaceOutput(inputPath, outputPath, "mvedit-replace")
		if err != nil {
			return 0, err
		}
		defer cleanupTmp()
	}

	dir := filepath.Dir(outPath)

	partA, err := tempLike(outPath, "mvedit-partA")
	if err != nil {
		return 0, err
	}
	defer os.Remove(partA)

	partB, err := tempLike(outPath, "mvedit-partB")
	if err != nil {
		return 0, err
	}
	defer os.Remove(partB)

	if err := CopySection(ctx, inputPath, partA, 0, startMs); err != nil {
		return 0, err
	}
	if err := CopySection(ctx, inputPath, partB, endMs, inputDurationMs); err != nil {
		return 0, err
	}

	partADurationMs, err := probeDurationMs(ctx, partA)
	if err != nil {
		return 0, err
	}
	newEndMs := partADurationMs + replaceDurationMs

	listPath, listCleanup, err := createConcatListFile(dir, []string{partA, replacePath, partB})
	if err != nil {
		return 0, err
	}
	defer listCleanup()

	if err := concatListCopyOrReencode(ctx, listPath, outPath); err != nil {
		return 0, err
	}

	if err := finalize(); err != nil {
		return 0, err
	}

	return newEndMs, nil
}
