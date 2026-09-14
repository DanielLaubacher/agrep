package main

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/DanielLaubacher/agrep/internal/cli"
)

const usage = `Usage: agrep [OPTIONS] PATTERN [FILE...]

A high-performance, Linux-focused search tool for pattern matching in files.

Options:
  -e, --regexp PATTERN     Pattern to match (can be specified multiple times)
  -F, --fixed-strings      Interpret pattern as fixed string, not regex
  -P, --perl-regexp        Interpret pattern as PCRE2 regex
  -t, --pipe               Pipe matched text into the next -e pattern
  -o, --only-matching      Print only the matched part of the line
  -i, --ignore-case        Case-insensitive matching
  -S, --smart-case         Case-insensitive if pattern is all lowercase
  -v, --invert-match       Select non-matching lines
  -n, --line-number        Print line numbers
  -c, --count              Print only match count per file
  -l, --files-with-matches Print only filenames with matches
  -r, --recursive          Recursively search directories
      --json               Output results as JSON Lines
  -B, --before-context NUM Print NUM lines before match
  -A, --after-context NUM  Print NUM lines after match
  -C, --context NUM        Print NUM lines before and after match
  -M, --max-columns NUM    Truncate lines longer than NUM bytes (0=auto, -1=no limit)
  -U, --multiline          Patterns may match across lines; output spans the
                           whole matched block (regex only; ^ $ match per line)
      --structural         PATTERN is a structural template with :[name] holes
                           matching lazily within balanced delimiters
      --lang NAME          Language family for -S string/comment handling
                           (go py js c rs sh rb md; default: generic)
      --capture NAME       With -S --histogram: aggregate the named hole's
                           captured text instead of full matches
      --block              Emit each match's whole enclosing definition block
                           (function/class body; Markdown section)
      --color MODE         Use color: always, never, auto (default: auto)
      --colour MODE        Alias for --color
  -g, --glob PATTERN       Include/exclude files by glob (prefix ! to exclude)
      --no-ignore          Don't respect .gitignore files
      --hidden             Search hidden files and directories
  -L, --follow             Follow symbolic links
      --watch              Watch files for changes and search new content
  -h, --help               Show this help

Agent options (see agent-mode.md):
      --ident              Match the pattern as an identifier: all case
                           conventions (camel/snake/kebab/SCREAMING), word-bounded
      --max-tokens N       Budget output to ~N tokens; report what was omitted
      --outline            Per-file survey (count + exemplar line), busiest first
      --top K              Limit --outline/--histogram to the K busiest entries
      --histogram          Count distinct matched texts (uniq -c built in);
                           composes with -o pipelines
      --rank MODE          --outline order: count (default) or density
                           (matches/KB, demotes vendored/generated files)
      --collapse           Suppress repeats of identical match lines after 3
                           occurrences; reports exactly what was collapsed
      --sections           Annotate matches with their Markdown section heading
      --scope              Annotate matches with their enclosing definition
                           (func/class/def by language; headings in Markdown)
      --batch FILE         Run all patterns in FILE (one per line) in one pass
      --files-from FILE    Search the files listed in FILE ('-' = stdin),
                           one path per line, instead of walking
      --changed-since REF  Search only files changed since the git REF
                           (plus untracked files; requires git)
      --with-file PAT      Only report files that also contain PAT (repeatable)
      --without-file PAT   Only report files that do not contain PAT (repeatable)
      --suggest            On zero hits, probe derived variants and report
                           counts (always reports, even when nothing occurs)
      --get-region SPAN    Print exact bytes for a "path@start-end" span id,
                           or whole lines for "path@:120-160" (1-based)
      --expand N           Widen --get-region by N whole lines each side
      --use-index          Build/use a trigram index for recursive search;
                           auto-refreshed by a stat sweep on every query
      --clear-index PATH   Delete index state for every root at/under PATH
      --skill              Print agent operating instructions (workflow,
                           JSON contract, citation/verification loop)

Pipeline:
  Flags -F, -P, -t, -o are per-stage modifiers that apply to the next -e.
  Use -t to pipe matched text from one pattern into the next (match narrowing).
  Combine short flags freely: -Ftoe 'pattern' = fixed + pipe + only-match.

  Examples:
    agrep -Fe 'ERROR' -toe '\d+'          # SIMD prefilter, then extract digits
    agrep -Fe 'HTTP' -te 'status=\d+' -toe '\d+'  # three-stage narrowing
    agrep -oe '\d+' file.log              # only-matching (like grep -o)
`

// profileFlags holds the --cpuprofile/--memprofile targets, which are
// handled by main rather than the search pipeline.
type profileFlags struct {
	cpu string
	mem string
}

// parseArgs merges config-file arguments with argv, parses all flags and
// positionals, applies defaults, and validates. It prints usage and exits
// for -h (or a missing pattern), and exits with code 2 on invalid input.
func parseArgs(args []string) (cli.Config, profileFlags) {
	var cfg cli.Config
	var prof profileFlags
	var colorFlag string
	var contextLines int
	var showHelp bool

	// Per-stage modifier accumulators (reset after each -e)
	var stageFixed bool
	var stagePCRE bool
	var stagePipe bool
	var stageOnly bool

	// Track whether -e was used explicitly
	explicitE := false

	// Merge config file args with CLI args
	cliArgs := args
	if configArgs := cli.LoadConfigArgs(); len(configArgs) > 0 {
		cliArgs = append(configArgs, cliArgs...)
	}

	// Manual flag parsing to support both -x and --xxx forms, plus positional args
	var positional []string
	for i := 0; i < len(cliArgs); i++ {
		arg := cliArgs[i]

		// Stop flag parsing at "--"
		if arg == "--" {
			positional = append(positional, cliArgs[i+1:]...)
			break
		}

		// Not a flag — positional arg
		if !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}

		// Parse flag=value form
		key, val, hasEq := splitFlag(arg)

		// Helper to get value: either from =val or next arg
		nextVal := func() (string, bool) {
			if hasEq {
				return val, true
			}
			if i+1 < len(cliArgs) {
				i++
				return cliArgs[i], true
			}
			return "", false
		}

		switch key {
		case "-e", "--regexp":
			v, ok := nextVal()
			if !ok {
				die("flag %s requires a value", key)
			}
			explicitE = true

			stage := cli.StageConfig{
				Pattern:   v,
				Fixed:     stageFixed,
				PCRE:      stagePCRE,
				Pipe:      stagePipe,
				OnlyMatch: stageOnly,
			}

			if stagePipe {
				// Append to current pipeline
				if len(cfg.Pipelines) == 0 {
					die("-t on first pattern has nothing to pipe from")
				}
				last := len(cfg.Pipelines) - 1
				cfg.Pipelines[last] = append(cfg.Pipelines[last], stage)
			} else {
				// Start a new pipeline (OR branch)
				cfg.Pipelines = append(cfg.Pipelines, []cli.StageConfig{stage})
			}

			// Also populate legacy Patterns for backwards compat
			cfg.Patterns = append(cfg.Patterns, v)

			// Reset per-stage modifiers
			stageFixed = false
			stagePCRE = false
			stagePipe = false
			stageOnly = false

		case "-g", "--glob":
			v, ok := nextVal()
			if !ok {
				die("flag %s requires a value", key)
			}
			cfg.Globs = append(cfg.Globs, v)
		case "-F", "--fixed-strings":
			stageFixed = true
		case "-P", "--perl-regexp":
			stagePCRE = true
		case "-t", "--pipe":
			stagePipe = true
		case "-o", "--only-matching":
			stageOnly = true
		case "-i", "--ignore-case":
			cfg.IgnoreCase = true
		case "-S", "--smart-case":
			cfg.SmartCase = true
		case "-v", "--invert-match":
			cfg.Invert = true
		case "-n", "--line-number":
			cfg.LineNumbers = true
		case "-c", "--count":
			cfg.CountOnly = true
		case "-l", "--files-with-matches":
			cfg.FileNamesOnly = true
		case "-r", "--recursive":
			cfg.Recursive = true
		case "--json":
			cfg.JSONOutput = true
		case "--no-ignore":
			cfg.NoIgnore = true
		case "--hidden":
			cfg.Hidden = true
		case "-L", "--follow":
			cfg.FollowSymlinks = true
		case "--watch":
			cfg.WatchMode = true
		case "-U", "--multiline":
			cfg.Multiline = true
		case "--structural":
			cfg.Structural = true
		case "--lang":
			v, ok := nextVal()
			if !ok {
				die("flag %s requires a value", key)
			}
			cfg.Lang = v
		case "--capture":
			v, ok := nextVal()
			if !ok {
				die("flag %s requires a value", key)
			}
			cfg.Capture = v
		case "--block":
			cfg.Block = true
		case "-h", "--help":
			showHelp = true
		case "--skill":
			fmt.Print(skillText)
			os.Exit(0)
		case "-B", "--before-context":
			v, ok := nextVal()
			if !ok {
				die("flag %s requires a value", key)
			}
			cfg.ContextBefore = atoi(v, key)
		case "-A", "--after-context":
			v, ok := nextVal()
			if !ok {
				die("flag %s requires a value", key)
			}
			cfg.ContextAfter = atoi(v, key)
		case "-C", "--context":
			v, ok := nextVal()
			if !ok {
				die("flag %s requires a value", key)
			}
			contextLines = atoi(v, key)
		case "-M", "--max-columns":
			v, ok := nextVal()
			if !ok {
				die("flag %s requires a value", key)
			}
			cfg.MaxColumns = atoi(v, key)
		case "--color", "--colour":
			v, ok := nextVal()
			if !ok {
				die("flag %s requires a value", key)
			}
			colorFlag = v
		case "--cpuprofile":
			v, ok := nextVal()
			if !ok {
				die("flag %s requires a value", key)
			}
			prof.cpu = v
		case "--max-tokens":
			v, ok := nextVal()
			if !ok {
				die("flag %s requires a value", key)
			}
			cfg.MaxTokens = atoi(v, key)
		case "--outline":
			cfg.Outline = true
		case "--histogram":
			cfg.Histogram = true
		case "--rank":
			v, ok := nextVal()
			if !ok {
				die("flag %s requires a value", key)
			}
			if v != "count" && v != "density" {
				die("--rank must be count or density, got %q", v)
			}
			cfg.Rank = v
		case "--top":
			v, ok := nextVal()
			if !ok {
				die("flag %s requires a value", key)
			}
			cfg.TopK = atoi(v, key)
		case "--sections":
			cfg.Sections = true
		case "--collapse":
			cfg.Collapse = true
		case "--scope":
			cfg.Scope = true
		case "--batch":
			v, ok := nextVal()
			if !ok {
				die("flag %s requires a value", key)
			}
			cfg.BatchFile = v
		case "--files-from":
			v, ok := nextVal()
			if !ok {
				die("flag %s requires a value", key)
			}
			cfg.FilesFrom = v
		case "--changed-since":
			v, ok := nextVal()
			if !ok {
				die("flag %s requires a value", key)
			}
			cfg.ChangedSince = v
		case "--with-file":
			v, ok := nextVal()
			if !ok {
				die("flag %s requires a value", key)
			}
			cfg.WithFile = append(cfg.WithFile, v)
		case "--without-file":
			v, ok := nextVal()
			if !ok {
				die("flag %s requires a value", key)
			}
			cfg.WithoutFile = append(cfg.WithoutFile, v)
		case "--suggest":
			cfg.Suggest = true
		case "--ident":
			cfg.Ident = true
		case "--use-index":
			cfg.UseIndex = true
		case "--clear-index":
			v, ok := nextVal()
			if !ok {
				die("flag %s requires a value", key)
			}
			cfg.ClearIndex = v
		case "--get-region":
			v, ok := nextVal()
			if !ok {
				die("flag %s requires a value", key)
			}
			cfg.GetRegion = v
		case "--expand":
			v, ok := nextVal()
			if !ok {
				die("flag %s requires a value", key)
			}
			cfg.ExpandLines = atoi(v, key)
		case "--memprofile":
			v, ok := nextVal()
			if !ok {
				die("flag %s requires a value", key)
			}
			prof.mem = v
		default:
			// Try to expand combined short flags like -rin, -Ftoe, -il
			if len(key) > 2 && key[0] == '-' && key[1] != '-' {
				// Expand: inject individual flags back
				expanded := make([]string, 0, len(key)-1)
				for _, ch := range key[1:] {
					expanded = append(expanded, "-"+string(ch))
				}
				// Replace current position with expanded flags
				rest := make([]string, 0, len(expanded)+len(cliArgs)-i-1)
				rest = append(rest, expanded...)
				rest = append(rest, cliArgs[i+1:]...)
				cliArgs = append(cliArgs[:i], rest...)
				i-- // re-process from current position
				continue
			}
			die("unknown flag: %s", key)
		}
	}

	if showHelp {
		fmt.Print(usage)
		os.Exit(0)
	}

	// Parse positional args: first arg is pattern (if -e not used), rest are files.
	// --get-region and --batch supply their own work, so no pattern is required
	// and every positional is a path.
	if cfg.GetRegion != "" || cfg.BatchFile != "" || cfg.ClearIndex != "" {
		cfg.Paths = positional
	} else if !explicitE {
		if len(positional) == 0 {
			fmt.Print(usage)
			os.Exit(0)
		}
		pattern := positional[0]
		cfg.Patterns = []string{pattern}
		cfg.Paths = positional[1:]

		// Build pipeline from positional pattern with accumulated modifiers
		stage := cli.StageConfig{
			Pattern:   pattern,
			Fixed:     stageFixed,
			PCRE:      stagePCRE,
			OnlyMatch: stageOnly,
		}
		cfg.Pipelines = [][]cli.StageConfig{{stage}}
	} else {
		cfg.Paths = positional
	}

	// Set legacy Fixed/PCRE for backwards compat (single pipeline, single stage)
	if len(cfg.Pipelines) == 1 && len(cfg.Pipelines[0]) == 1 {
		cfg.Fixed = cfg.Pipelines[0][0].Fixed
		cfg.PCRE = cfg.Pipelines[0][0].PCRE
		cfg.OnlyMatch = cfg.Pipelines[0][0].OnlyMatch
	}

	// Auto-recurse: if any path is a directory, enable recursive mode.
	// If no paths given and stdin is a terminal, default to current directory.
	if len(cfg.Paths) == 0 && !cfg.WatchMode {
		if isTerminal(unix.Stdin) {
			cfg.Paths = []string{"."}
			cfg.Recursive = true
		}
	}
	if !cfg.Recursive {
		for _, p := range cfg.Paths {
			var stat unix.Stat_t
			if unix.Stat(p, &stat) == nil && stat.Mode&unix.S_IFMT == unix.S_IFDIR {
				cfg.Recursive = true
				break
			}
		}
	}

	// Handle -C (sets both before and after)
	if contextLines > 0 {
		if cfg.ContextBefore == 0 {
			cfg.ContextBefore = contextLines
		}
		if cfg.ContextAfter == 0 {
			cfg.ContextAfter = contextLines
		}
	}

	// Parse color mode
	switch colorFlag {
	case "always":
		cfg.Color = cli.ColorAlways
	case "never":
		cfg.Color = cli.ColorNever
	default:
		cfg.Color = cli.ColorAuto
	}

	// Set defaults
	if cfg.MmapThreshold == 0 {
		cfg.MmapThreshold = 8 * 1024 * 1024 // 8MB
	}

	if err := cfg.Validate(); err != nil {
		die("%v", err)
	}

	return cfg, prof
}

func isTerminal(fd int) bool {
	_, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	return err == nil
}

// splitFlag splits "--flag=value" into ("--flag", "value", true)
// or returns ("--flag", "", false) if no = present.
func splitFlag(arg string) (string, string, bool) {
	if before, after, ok := strings.Cut(arg, "="); ok {
		return before, after, true
	}
	return arg, "", false
}

func atoi(s string, flag string) int {
	n := 0
	neg := false
	i := 0
	if len(s) > 0 && s[0] == '-' {
		neg = true
		i = 1
	}
	for ; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			die("invalid integer value %q for flag %s", s, flag)
		}
		n = n*10 + int(s[i]-'0')
	}
	if neg {
		n = -n
	}
	return n
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "agrep: "+format+"\n", args...)
	os.Exit(2)
}
