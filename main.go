package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
)

const appVersion = "1.0.0"

const (
	defaultWarnBytes int64 = 50 * 1024 * 1024
	defaultFailBytes int64 = 100 * 1024 * 1024
)

type thresholds struct {
	Warn int64 `json:"warn_bytes"`
	Fail int64 `json:"fail_bytes"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "repo-size-guard:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return fmt.Errorf("usage: repo-size-guard <version|scan|hook>")
	}

	switch os.Args[1] {
	case "version":
		fmt.Printf("repo-size-guard %s\n", appVersion)
		return nil
	case "scan":
		return runScan(os.Args[2:])
	default:
		return fmt.Errorf("unknown command %q", os.Args[1])
	}
}

func runScan(args []string) error {
	t, rest, err := parseThresholds(args)
	if err != nil {
		return err
	}

	mode := "staged"
	paths := []string(nil)
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--staged":
			mode = "staged"
		case "--worktree":
			mode = "worktree"
		case "--":
			paths = append(paths, rest[i+1:]...)
			i = len(rest)
		default:
			return fmt.Errorf("unknown scan argument %q", rest[i])
		}
	}
	if mode == "staged" && len(paths) != 0 {
		return fmt.Errorf("staged scans do not accept worktree pathspecs")
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	var report Report
	if mode == "staged" {
		report, err = analyzeStaged(cwd, t)
	} else {
		report, err = analyzeWorktree(cwd, paths, t)
	}
	if err != nil {
		return err
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

func parseThresholds(args []string) (thresholds, []string, error) {
	t := thresholds{Warn: defaultWarnBytes, Fail: defaultFailBytes}
	rest := make([]string, 0, len(args))

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--warn-bytes", "--fail-bytes":
			if i+1 >= len(args) {
				return t, nil, fmt.Errorf("%s requires a value", args[i])
			}
			value, err := strconv.ParseInt(args[i+1], 10, 64)
			if err != nil || value <= 0 {
				return t, nil, fmt.Errorf("%s must be a positive integer", args[i])
			}
			if args[i] == "--warn-bytes" {
				t.Warn = value
			} else {
				t.Fail = value
			}
			i++
		default:
			rest = append(rest, args[i])
		}
	}
	if t.Warn >= t.Fail {
		return t, nil, fmt.Errorf("warn threshold must be smaller than fail threshold")
	}
	return t, rest, nil
}
