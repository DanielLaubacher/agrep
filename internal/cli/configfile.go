package cli

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// LoadConfigArgs reads the agrep config file and returns parsed arguments.
// Config file location: AGREP_CONFIG_PATH env var, ~/.agrep, or ~/.gogrep
// (pre-rename fallback so existing configs keep working).
// Format: one flag per line, # comments, empty lines ignored.
// Returns nil if no config file found.
func LoadConfigArgs() []string {
	var f *os.File
	if path := os.Getenv("AGREP_CONFIG_PATH"); path != "" {
		var err error
		if f, err = os.Open(path); err != nil {
			return nil
		}
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		for _, name := range []string{".agrep", ".gogrep"} {
			if f, err = os.Open(filepath.Join(home, name)); err == nil {
				break
			}
			f = nil
		}
		if f == nil {
			return nil
		}
	}
	defer f.Close()

	var args []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		args = append(args, line)
	}
	return args
}
