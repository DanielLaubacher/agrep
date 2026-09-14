package main

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"time"
)

// convertPDF extracts text via pdftotext -layout, converting its form-feed
// page separators into "<!-- p.N -->" markers so byte spans in the mirror
// can be cited back to physical pages in the original PDF.
func convertPDF(src string) ([]byte, int, string, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "pdftotext", "-layout", "-enc", "UTF-8", "-q", src, "-")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := stderr.String()
		if len(msg) > 120 {
			msg = msg[:120]
		}
		return nil, 0, "pdftotext", "", fmt.Errorf("pdftotext: %v %s", err, msg)
	}

	pagesRaw := bytes.Split(stdout.Bytes(), []byte{'\f'})
	var out bytes.Buffer
	out.Grow(stdout.Len() + len(pagesRaw)*16)
	pages := 0
	for i, p := range pagesRaw {
		p = bytes.TrimSpace(p)
		if len(p) == 0 {
			continue
		}
		pages++
		out.WriteString("<!-- p.")
		out.WriteString(strconv.Itoa(i + 1))
		out.WriteString(" -->\n")
		out.Write(p)
		out.WriteString("\n\n")
	}
	// Report the physical page count (including empty/image pages) so the
	// OCR heuristic sees the true denominator.
	return out.Bytes(), len(pagesRaw), "pdftotext -layout", "", nil
}
