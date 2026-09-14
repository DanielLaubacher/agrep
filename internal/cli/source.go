package cli

// fileSource centralizes where searched files come from: an explicit
// list (--files-from, '-' = stdin), the recursive walk, or literal
// paths. Every aggregation and search mode draws from this one place,
// so list-driven composition (`gogrep -l ... | gogrep --files-from -`)
// works everywhere.

import (
	"io"
	"os"
	"strings"

	"github.com/dl/gogrep/internal/walker"
)

// fileSource returns the channel of files to search. Walk errors are
// logged to stderr in the background.
func fileSource(cfg Config, paths []string) (<-chan walker.FileEntry, error) {
	if cfg.FilesFrom != "" {
		list, err := loadFileList(cfg.FilesFrom)
		if err != nil {
			return nil, err
		}
		ch := make(chan walker.FileEntry, len(list))
		for _, p := range list {
			ch <- walker.FileEntry{Path: p}
		}
		close(ch)
		return ch, nil
	}

	if cfg.Recursive {
		ch, errCh := walker.Walk(paths, walker.WalkOptions{
			Recursive:      true,
			NoIgnore:       cfg.NoIgnore,
			Hidden:         cfg.Hidden,
			FollowSymlinks: cfg.FollowSymlinks,
			Globs:          cfg.Globs,
		})
		go func() {
			for err := range errCh {
				logWarn("walk: %v", err)
			}
		}()
		return ch, nil
	}

	ch := make(chan walker.FileEntry, len(paths))
	for _, p := range paths {
		ch <- walker.FileEntry{Path: p}
	}
	close(ch)
	return ch, nil
}

// loadFileList reads one path per line ('-' = stdin); blank lines are
// skipped.
func loadFileList(from string) ([]string, error) {
	var data []byte
	var err error
	if from == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(from)
	}
	if err != nil {
		return nil, err
	}
	var list []string
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			list = append(list, line)
		}
	}
	return list, nil
}
