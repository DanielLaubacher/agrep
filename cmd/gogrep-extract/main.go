// Gogrep-extract mirrors a library of PDF/EPUB/MOBI files into a tree of
// plain-text Markdown files that gogrep (and agents driving it) can
// search. The mirror preserves the source directory structure, one .md
// per book, with front-matter provenance and page markers for citations.
// Sources are never written to.
//
// Extraction is incremental: a book is skipped when its mirror file
// already exists with the same modification time as the source (the tool
// sets each mirror file's mtime to its source's mtime).
//
// A manifest.tsv at the mirror root records per-book status — including
// books that yielded no text (scanned PDFs needing OCR), so the corpus's
// blind spots are themselves greppable.
package main

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

type status string

const (
	statusOK        status = "ok"
	statusSkipped   status = "skipped"   // up to date from a previous run
	statusEmpty     status = "empty"     // extraction produced no text
	statusOCRNeeded status = "ocr"       // image-only PDF, needs OCR
	statusFailed    status = "failed"    // converter error
	statusIgnored   status = "ignored"   // not a supported book format
	statusDuplicate status = "duplicate" // same book in a preferred format
)

// formatRank orders formats by extraction quality: EPUB (native headings,
// real structure) beats PDF (page markers, layout-flattened) beats MOBI.
// When one book exists in several formats, only the best is extracted.
func formatRank(ext string) int {
	switch ext {
	case ".epub":
		return 0
	case ".pdf":
		return 1
	case ".mobi", ".azw", ".azw3":
		return 2
	}
	return 9
}

type entry struct {
	status    status
	rel       string
	outBytes  int
	pages     int
	extractor string
	note      string
}

func main() {
	src := flag.String("src", "", "source library root (read-only)")
	dst := flag.String("dst", "", "mirror destination root")
	workers := flag.Int("workers", runtime.NumCPU(), "parallel conversions")
	force := flag.Bool("force", false, "re-extract even if up to date")
	verbose := flag.Bool("v", false, "log each file")
	flag.Parse()
	if *src == "" || *dst == "" {
		fmt.Fprintln(os.Stderr, "usage: gogrep-extract -src LIBRARY -dst MIRROR [-workers N] [-force]")
		os.Exit(2)
	}

	type job struct {
		rel  string
		info fs.FileInfo
	}
	// Books sharing a directory and basename (book.epub + book.pdf) are
	// the same work in different formats: group them so only the best
	// format is extracted (with fallback to the next if it yields nothing).
	groups := map[string][]job{}
	err := filepath.WalkDir(*src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			fmt.Fprintf(os.Stderr, "walk: %s: %v\n", path, err)
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(*src, path)
		if err != nil {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		key := strings.TrimSuffix(rel, filepath.Ext(rel))
		groups[key] = append(groups[key], job{rel: rel, info: info})
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "walk: %v\n", err)
		os.Exit(2)
	}
	var jobs [][]job
	total := 0
	for _, g := range groups {
		sort.Slice(g, func(i, j int) bool {
			return formatRank(strings.ToLower(filepath.Ext(g[i].rel))) <
				formatRank(strings.ToLower(filepath.Ext(g[j].rel)))
		})
		jobs = append(jobs, g)
		total += len(g)
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i][0].rel < jobs[j][0].rel })

	var (
		mu      sync.Mutex
		entries []entry
		done    int
	)
	jobCh := make(chan []job)
	var wg sync.WaitGroup
	for range *workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for g := range jobCh {
				satisfied := false
				for _, j := range g {
					var e entry
					if satisfied && formatRank(strings.ToLower(filepath.Ext(j.rel))) < 9 {
						e = entry{status: statusDuplicate, rel: j.rel,
							note: "same book already extracted from a preferred format"}
					} else {
						e = extractOne(*src, *dst, j.rel, j.info, *force)
						if e.status == statusOK || e.status == statusSkipped {
							satisfied = true
						}
					}
					mu.Lock()
					entries = append(entries, e)
					done++
					if *verbose || done%100 == 0 {
						fmt.Fprintf(os.Stderr, "[%d/%d] %-9s %s\n", done, total, e.status, j.rel)
					}
					mu.Unlock()
				}
			}
		}()
	}
	start := time.Now()
	for _, g := range jobs {
		jobCh <- g
	}
	close(jobCh)
	wg.Wait()

	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })
	writeManifest(*dst, entries)

	counts := map[status]int{}
	totalBytes := 0
	for _, e := range entries {
		counts[e.status]++
		totalBytes += e.outBytes
	}
	fmt.Printf("done in %s: %d files — ok:%d skipped:%d duplicate:%d empty:%d ocr-needed:%d failed:%d ignored:%d — %.1f MB extracted\n",
		time.Since(start).Round(time.Second), len(entries),
		counts[statusOK], counts[statusSkipped], counts[statusDuplicate], counts[statusEmpty],
		counts[statusOCRNeeded], counts[statusFailed], counts[statusIgnored],
		float64(totalBytes)/1e6)
}

// extractOne converts a single source file into its mirror .md.
func extractOne(srcRoot, dstRoot, rel string, info fs.FileInfo, force bool) entry {
	e := entry{rel: rel}
	ext := strings.ToLower(filepath.Ext(rel))

	var convert func(src string) (body []byte, pages int, extractor, note string, err error)
	switch ext {
	case ".pdf":
		convert = convertPDF
	case ".epub":
		convert = convertEPUB
	case ".mobi", ".azw", ".azw3":
		convert = convertMOBI
	default:
		e.status = statusIgnored
		return e
	}

	srcPath := filepath.Join(srcRoot, rel)
	outRel := strings.TrimSuffix(rel, filepath.Ext(rel)) + ".md"
	outPath := filepath.Join(dstRoot, outRel)

	// Incremental: mirror exists with matching mtime and content → done.
	if !force {
		if st, err := os.Stat(outPath); err == nil && st.Size() > 0 &&
			st.ModTime().Equal(info.ModTime().Truncate(time.Second)) {
			e.status = statusSkipped
			e.outBytes = int(st.Size())
			return e
		}
	}

	body, pages, extractor, note, err := convert(srcPath)
	e.pages = pages
	e.extractor = extractor
	if err != nil {
		e.status = statusFailed
		e.note = err.Error()
		return e
	}

	trimmed := strings.TrimSpace(string(body))
	switch {
	case len(trimmed) == 0:
		e.status = statusEmpty
	case ext == ".pdf" && pages > 3 && len(trimmed)/max(pages, 1) < 100:
		// Pages exist but almost no text per page: image-only scan.
		e.status = statusOCRNeeded
		e.note = fmt.Sprintf("%d chars over %d pages", len(trimmed), pages)
	default:
		e.status = statusOK
	}
	if e.status != statusOK {
		return e
	}

	var out strings.Builder
	out.WriteString("---\n")
	fmt.Fprintf(&out, "source: %s\n", srcPath)
	fmt.Fprintf(&out, "source_size: %d\n", info.Size())
	fmt.Fprintf(&out, "source_mtime: %s\n", info.ModTime().UTC().Format(time.RFC3339))
	fmt.Fprintf(&out, "extractor: %s\n", extractor)
	if pages > 0 {
		fmt.Fprintf(&out, "pages: %d\n", pages)
	}
	if note != "" {
		fmt.Fprintf(&out, "title: %s\n", note)
	}
	out.WriteString("---\n\n")
	out.WriteString(trimmed)
	out.WriteString("\n")

	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		e.status = statusFailed
		e.note = err.Error()
		return e
	}
	if err := os.WriteFile(outPath, []byte(out.String()), 0o644); err != nil {
		e.status = statusFailed
		e.note = err.Error()
		return e
	}
	// Stamp the source's mtime for incremental runs and recency queries.
	mt := info.ModTime().Truncate(time.Second)
	os.Chtimes(outPath, mt, mt)
	e.outBytes = out.Len()
	return e
}

func writeManifest(dstRoot string, entries []entry) {
	var b strings.Builder
	b.WriteString("status\tpath\tbytes\tpages\textractor\tnote\n")
	for _, e := range entries {
		note := strings.ReplaceAll(e.note, "\t", " ")
		note = strings.ReplaceAll(note, "\n", " ")
		fmt.Fprintf(&b, "%s\t%s\t%d\t%d\t%s\t%s\n",
			e.status, e.rel, e.outBytes, e.pages, e.extractor, note)
	}
	if err := os.WriteFile(filepath.Join(dstRoot, "manifest.tsv"), []byte(b.String()), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "manifest: %v\n", err)
	}
}
