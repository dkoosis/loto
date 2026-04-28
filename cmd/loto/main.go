// Command loto is a small CLI that exercises the loto library.
//
// Usage:
//
//	loto [flags] file <target>     acquire and hold a file lock until SIGINT
//	loto [flags] global            acquire and hold the global lock until SIGINT
//	loto status [target...]        show tag for global, or for given files
//	loto break <target>            force-release a stale file lock
//
// Flags:
//
//	-base    coordination base directory (or $LOTO_BASE; default ./.loto)
//	-agent   agent id (default pid-N)
//	-intent  human-readable intent
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"loto"
)

func main() {
	baseDir := flag.String("base", defaultBase(), "coordination base directory")
	agent := flag.String("agent", fmt.Sprintf("pid-%d", os.Getpid()), "agent id")
	intent := flag.String("intent", "ad-hoc", "human-readable intent")
	flag.Parse()

	if flag.NArg() < 1 {
		usage()
		os.Exit(2)
	}

	l, err := loto.New(*baseDir)
	if err != nil {
		fail(err)
	}

	switch flag.Arg(0) {
	case "file":
		if flag.NArg() < 2 {
			usage()
			os.Exit(2)
		}
		holdFile(l, *agent, *intent, flag.Arg(1))
	case "global":
		holdGlobal(l, *agent, *intent)
	case "status":
		showStatus(l, flag.Args()[1:])
	case "break":
		if flag.NArg() < 2 {
			usage()
			os.Exit(2)
		}
		if err := l.Break(flag.Arg(1)); err != nil {
			fail(err)
		}
		fmt.Println("ok")
	default:
		usage()
		os.Exit(2)
	}
}

func holdFile(l *loto.LOTO, agent, intent, target string) {
	lock, err := l.TryFileLock(agent, intent, target)
	if err != nil {
		fail(err)
	}
	defer func() { _ = lock.Unlock() }()
	fmt.Printf("file lock acquired: %s\n", target)
	waitForSignal()
}

func holdGlobal(l *loto.LOTO, agent, intent string) {
	lock, err := l.TryGlobalLock(agent, intent)
	if err != nil {
		fail(err)
	}
	defer func() { _ = lock.Unlock() }()
	fmt.Println("global lock acquired")
	waitForSignal()
}

func showStatus(l *loto.LOTO, targets []string) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if len(targets) == 0 {
		tag, err := l.ReadGlobalTag()
		if err != nil {
			fmt.Println("global: free")
			return
		}
		fmt.Println("global:")
		_ = enc.Encode(tag)
		return
	}
	for _, t := range targets {
		tag, err := l.ReadTag(t)
		if err != nil {
			fmt.Printf("%s: free\n", t)
			continue
		}
		fmt.Printf("%s:\n", t)
		_ = enc.Encode(tag)
	}
}

func waitForSignal() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	<-c
}

func defaultBase() string {
	if v := os.Getenv("LOTO_BASE"); v != "" {
		return v
	}
	cwd, err := os.Getwd()
	if err != nil {
		return ".loto"
	}
	return filepath.Join(cwd, ".loto")
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: loto [flags] <command> [args]

commands:
  file <target>      acquire and hold a file lock until SIGINT
  global             acquire and hold the global lock until SIGINT
  status [target..]  show tag for global lock, or for given files
  break <target>     force-release a stale file lock (must be unheld)

flags:
  -base    coordination base directory (or $LOTO_BASE; default ./.loto)
  -agent   agent id (default pid-N)
  -intent  human-readable intent`)
}
