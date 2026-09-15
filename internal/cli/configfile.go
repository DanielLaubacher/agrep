package cli

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// LoadConfigArgs reads the agrep config file and returns parsed
// arguments plus the file's path (for attribution in warnings).
// Config file location: AGREP_CONFIG_PATH env var, or ~/.agrep.
// Format: one flag per line, # comments, empty lines ignored.
// Returns nil if no config file found.
func LoadConfigArgs() ([]string, string) {
	var f *os.File
	if path := os.Getenv("AGREP_CONFIG_PATH"); path != "" {
		var err error
		if f, err = os.Open(path); err != nil {
			return nil, ""
		}
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, ""
		}
		if f, err = os.Open(filepath.Join(home, ".agrep")); err != nil {
			return nil, ""
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
	return args, f.Name()
}
