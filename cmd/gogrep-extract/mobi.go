package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// convertMOBI shells out to Calibre's ebook-convert (MOBI/AZW compression
// variants are not worth reimplementing). ebook-convert requires an output
// file, so a temp file bridges to the in-memory pipeline.
func convertMOBI(src string) ([]byte, int, string, string, error) {
	tmp, err := os.CreateTemp("", "gogrep-extract-*.txt")
	if err != nil {
		return nil, 0, "ebook-convert", "", err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ebook-convert", src, tmpPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		msg := string(out)
		if len(msg) > 120 {
			msg = msg[len(msg)-120:]
		}
		return nil, 0, "ebook-convert", "", fmt.Errorf("ebook-convert: %v %s", err, filepath.Base(msg))
	}
	data, err := os.ReadFile(tmpPath)
	if err != nil {
		return nil, 0, "ebook-convert", "", err
	}
	return data, 0, "ebook-convert", "", nil
}
