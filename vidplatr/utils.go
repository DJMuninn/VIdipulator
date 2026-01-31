package vidplatr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type ffprobeFormat struct {
	Duration string `json:"duration"`
}

type ffprobeOut struct {
	Format ffprobeFormat `json:"format"`
}

func probeDurationMs(ctx context.Context, filePath string) (int64, error) {
	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "error",
		"-print_format", "json",
		"-show_entries", "format=duration",
		filePath,
	)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return 0, errors.New("ffprobe failed: " + msg)
	}

	var out ffprobeOut
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return 0, err
	}

	secStr := strings.TrimSpace(out.Format.Duration)
	if secStr == "" {
		return 0, errors.New("ffprobe returned empty duration")
	}

	secs, err := strconv.ParseFloat(secStr, 64)
	if err != nil {
		return 0, err
	}

	ms := int64(secs * 1000)
	if ms < 0 {
		ms = 0
	}
	return ms, nil
}

func ensureParentDir(dstPath string) error {
	parent := filepath.Dir(dstPath)
	if parent == "." || parent == "" {
		return nil
	}
	return os.MkdirAll(parent, 0o755)
}

func tempLike(path string, prefix string) (string, error) {
	dir := filepath.Dir(path)
	ext := filepath.Ext(path)
	f, err := os.CreateTemp(dir, prefix+"-*"+ext)
	if err != nil {
		return "", err
	}
	name := f.Name()
	_ = f.Close()
	return name, nil
}

func prepareInPlaceOutput(inputPath, outputPath, prefix string) (outPath string, finalize func() error, cleanup func(), err error) {
	outPath = outputPath
	finalize = func() error { return nil }
	cleanup = func() {}

	if outputPath != inputPath {
		return outPath, finalize, cleanup, nil
	}

	tmp, err := tempLike(outputPath, prefix)
	if err != nil {
		return "", nil, nil, err
	}
	outPath = tmp
	cleanup = func() { _ = os.Remove(tmp) }
	finalize = func() error { return os.Rename(tmp, outputPath) }
	return outPath, finalize, cleanup, nil
}

func createConcatListFile(dir string, paths []string) (string, func(), error) {
	if len(paths) == 0 {
		return "", nil, errors.New("no concat paths")
	}
	for _, p := range paths {
		if strings.TrimSpace(p) == "" {
			return "", nil, errors.New("concat path is empty")
		}
		if strings.ContainsAny(p, "'\n\r") {
			return "", nil, errors.New("concat paths contain unsupported characters")
		}
	}

	f, err := os.CreateTemp(dir, "mvedit-concat-*.txt")
	if err != nil {
		return "", nil, err
	}
	listPath := f.Name()
	_ = f.Close()
	cleanup := func() { _ = os.Remove(listPath) }

	var b strings.Builder
	for _, p := range paths {
		b.WriteString("file '")
		b.WriteString(p)
		b.WriteString("'\n")
	}
	if err := os.WriteFile(listPath, []byte(b.String()), 0o600); err != nil {
		cleanup()
		return "", nil, err
	}

	return listPath, cleanup, nil
}

func concatListCopyOrReencode(ctx context.Context, listPath, outPath string) error {
	ffmpegCopyArgs := []string{
		"-y",
		"-hide_banner",
		"-loglevel", "error",
		"-f", "concat",
		"-safe", "0",
		"-i", listPath,
		"-map", "0:v:0",
		"-map", "0:a?",
		"-c", "copy",
		"-avoid_negative_ts", "make_zero",
	}
	ffmpegCopyArgs = append(ffmpegCopyArgs, movflags(outPath)...)
	ffmpegCopyArgs = append(ffmpegCopyArgs, outPath)

	copyErrOutput, copyErr := runFFmpeg(ctx, ffmpegCopyArgs...)
	if copyErr == nil {
		return nil
	}

	ffmpegReencodeArgs := []string{
		"-y",
		"-hide_banner",
		"-loglevel", "error",
		"-f", "concat",
		"-safe", "0",
		"-i", listPath,
		"-map", "0:v:0",
		"-map", "0:a?",
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-crf", "20",
		"-pix_fmt", "yuv420p",
		"-c:a", "aac",
		"-b:a", "192k",
	}
	ffmpegReencodeArgs = append(ffmpegReencodeArgs, movflags(outPath)...)
	ffmpegReencodeArgs = append(ffmpegReencodeArgs, outPath)

	reencodeErrOutput, reencodeErr := runFFmpeg(ctx, ffmpegReencodeArgs...)
	if reencodeErr != nil {
		return errors.New(strings.TrimSpace("concat-copy failed: " + copyErrOutput + "\nre-encode failed: " + reencodeErrOutput))
	}

	return nil
}

func movflags(dstPath string) []string {
	ext := strings.ToLower(filepath.Ext(dstPath))
	switch ext {
	case ".mp4", ".m4v", ".mov":
		return []string{"-movflags", "+faststart"}
	default:
		return nil
	}
}

func formatTimestampMs(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	// ffmpeg accepts timestamps like HH:MM:SS.mmm
	d := time.Duration(ms) * time.Millisecond
	h := int(d / time.Hour)
	d -= time.Duration(h) * time.Hour
	m := int(d / time.Minute)
	d -= time.Duration(m) * time.Minute
	s := int(d / time.Second)
	d -= time.Duration(s) * time.Second
	millis := int(d / time.Millisecond)
	return pad2(h) + ":" + pad2(m) + ":" + pad2(s) + "." + pad3(millis)
}

func pad2(v int) string {
	return fmt.Sprintf("%02d", v)
}

func pad3(v int) string {
	return fmt.Sprintf("%03d", v)
}

func runFFmpeg(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)

	var stderr bytes.Buffer
	cmd.Stdout = &stderr
	cmd.Stderr = &stderr

	err := cmd.Run()
	out := strings.TrimSpace(stderr.String())
	if err != nil {
		if out == "" {
			out = err.Error()
		}
		return out, errors.New("ffmpeg failed: " + out)
	}
	return out, nil
}
