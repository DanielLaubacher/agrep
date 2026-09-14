package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"os/exec"
	"path"
	"strings"
	"time"
)

// convertEPUB extracts an EPUB natively: it is a ZIP of XHTML chapters
// referenced, in reading order, by an OPF "spine". Headings become
// Markdown '#' headings (so gogrep --sections works on the mirror);
// everything else becomes plain paragraphs. Falls back to pandoc when the
// native path yields nothing useful.
func convertEPUB(src string) ([]byte, int, string, string, error) {
	body, chapters, title, err := epubNative(src)
	if err == nil && len(bytes.TrimSpace(body)) >= 500 {
		return body, chapters, "epub-native", title, nil
	}
	// Malformed container or exotic markup: let pandoc try.
	if pb, perr := epubPandoc(src); perr == nil && len(bytes.TrimSpace(pb)) > 0 {
		return pb, 0, "pandoc", title, nil
	}
	if err == nil {
		err = fmt.Errorf("no text extracted")
	}
	return nil, 0, "epub-native", "", err
}

func epubNative(src string) (body []byte, chapters int, title string, err error) {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return nil, 0, "", fmt.Errorf("zip: %w", err)
	}
	defer zr.Close()

	files := make(map[string]*zip.File, len(zr.File))
	for _, f := range zr.File {
		files[f.Name] = f
	}

	readAll := func(name string) ([]byte, error) {
		f, ok := files[name]
		if !ok {
			return nil, fmt.Errorf("missing %s", name)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(rc)
	}

	// META-INF/container.xml points at the OPF package file.
	containerData, err := readAll("META-INF/container.xml")
	if err != nil {
		return nil, 0, "", err
	}
	var container struct {
		Rootfiles []struct {
			FullPath string `xml:"full-path,attr"`
		} `xml:"rootfiles>rootfile"`
	}
	if err := xml.Unmarshal(containerData, &container); err != nil || len(container.Rootfiles) == 0 {
		return nil, 0, "", fmt.Errorf("container.xml: %v", err)
	}
	opfPath := container.Rootfiles[0].FullPath
	opfDir := path.Dir(opfPath)

	opfData, err := readAll(opfPath)
	if err != nil {
		return nil, 0, "", err
	}
	var opf struct {
		Title    []string `xml:"metadata>title"`
		Manifest []struct {
			ID   string `xml:"id,attr"`
			Href string `xml:"href,attr"`
		} `xml:"manifest>item"`
		Spine []struct {
			IDRef string `xml:"idref,attr"`
		} `xml:"spine>itemref"`
	}
	if err := xml.Unmarshal(opfData, &opf); err != nil {
		return nil, 0, "", fmt.Errorf("opf: %w", err)
	}
	if len(opf.Title) > 0 {
		title = strings.TrimSpace(opf.Title[0])
	}
	hrefByID := make(map[string]string, len(opf.Manifest))
	for _, item := range opf.Manifest {
		hrefByID[item.ID] = item.Href
	}

	var out bytes.Buffer
	for _, ref := range opf.Spine {
		href, ok := hrefByID[ref.IDRef]
		if !ok {
			continue
		}
		name := path.Clean(path.Join(opfDir, href))
		data, err := readAll(name)
		if err != nil {
			continue
		}
		text := xhtmlToMarkdown(data)
		if strings.TrimSpace(text) == "" {
			continue
		}
		chapters++
		out.WriteString(text)
		out.WriteString("\n\n")
	}
	return out.Bytes(), chapters, title, nil
}

// xhtmlToMarkdown flattens one XHTML chapter: h1..h6 become '#' headings,
// block elements become paragraphs, scripts/styles are dropped, and
// whitespace is normalized.
func xhtmlToMarkdown(data []byte) string {
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = false
	dec.AutoClose = xml.HTMLAutoClose
	dec.Entity = xml.HTMLEntity

	var out strings.Builder
	var text strings.Builder
	headingDepth := 0 // >0 while inside <hN>
	skipDepth := 0    // >0 while inside script/style/head

	flush := func() {
		t := strings.Join(strings.Fields(text.String()), " ")
		text.Reset()
		if t == "" {
			return
		}
		if headingDepth > 0 {
			out.WriteString(strings.Repeat("#", headingDepth))
			out.WriteByte(' ')
		}
		out.WriteString(t)
		out.WriteString("\n\n")
	}

	for {
		tok, err := dec.Token()
		if err != nil {
			break // io.EOF or malformed tail: keep what we have
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch name := strings.ToLower(t.Name.Local); name {
			case "script", "style", "head", "title":
				skipDepth++
			case "h1", "h2", "h3", "h4", "h5", "h6":
				flush()
				headingDepth = int(name[1] - '0')
			case "p", "div", "li", "tr", "blockquote", "pre", "section", "article", "table", "figcaption", "dt", "dd":
				flush()
			case "br":
				text.WriteByte(' ')
			}
		case xml.EndElement:
			switch name := strings.ToLower(t.Name.Local); name {
			case "script", "style", "head", "title":
				if skipDepth > 0 {
					skipDepth--
				}
			case "h1", "h2", "h3", "h4", "h5", "h6":
				flush()
				headingDepth = 0
			case "p", "div", "li", "tr", "blockquote", "pre", "section", "article", "table", "figcaption", "dt", "dd":
				flush()
			}
		case xml.CharData:
			if skipDepth == 0 {
				text.Write(t)
				text.WriteByte(' ')
			}
		}
	}
	flush()
	return out.String()
}

func epubPandoc(src string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "pandoc", "-f", "epub", "-t", "gfm", "--wrap=none", src, "-o", "-")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return stdout.Bytes(), nil
}
