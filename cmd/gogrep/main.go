// Gogrep is a high-performance, Linux-only grep alternative.
//
// It searches files and directories for patterns using SIMD-accelerated
// fixed-string search, a custom lazy-DFA regex engine, and raw Linux
// syscalls throughout. See the repository's architecture.md for design
// details and flags.go for the command-line surface.
//
// Exit codes follow grep convention: 0 = match found, 1 = no match,
// 2 = error.
package main

import (
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"runtime/pprof"

	"golang.org/x/sys/unix"

	"github.com/dl/gogrep/internal/cli"
)

func main() {
	cfg, prof := parseArgs(os.Args[1:])

	// CPU profiling support
	var profileFile *os.File
	if prof.cpu != "" {
		f, err := os.Create(prof.cpu)
		if err != nil {
			die("cpuprofile: %v", err)
		}
		profileFile = f
		pprof.StartCPUProfile(f)
	}

	// A one-shot grep process should essentially never garbage-collect:
	// GOGC=100 on a tiny cold heap triggers cycles worth ~1ms per run.
	// Defer collection until a generous soft limit instead. Skipped for
	// watch mode (long-running) and when the user sets GOGC explicitly.
	if os.Getenv("GOGC") == "" && !cfg.WatchMode {
		debug.SetGCPercent(-1)
		debug.SetMemoryLimit(256 << 20)
	}

	// Set up signal handling for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, unix.SIGINT, unix.SIGTERM)
	go func() {
		<-sigCh
		if profileFile != nil {
			pprof.StopCPUProfile()
			profileFile.Close()
		}
		os.Exit(130) // 128 + SIGINT
	}()

	exitCode := cli.Run(cfg)
	if profileFile != nil {
		pprof.StopCPUProfile()
		profileFile.Close()
	}
	if prof.mem != "" {
		f, err := os.Create(prof.mem)
		if err == nil {
			runtime.GC()
			pprof.WriteHeapProfile(f)
			f.Close()
		}
	}
	os.Exit(exitCode)
}
