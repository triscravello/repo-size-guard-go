package main

import "testing"

func TestClassifySupportedCommands(t *testing.T) {
	cases := []struct {
		command string
		kind    actionKind
	}{
		{"git add .", actionWorktree},
		{"git add -A", actionWorktree},
		{"git add -- \"assets/demo file.bin\"", actionWorktree},
		{"git commit -m \"portfolio release\"", actionStaged},
	}
	for _, tc := range cases {
		if got := classifyCommand(tc.command); got.Kind != tc.kind {
			t.Fatalf("%q: got %s, want %s", tc.command, got.Kind, tc.kind)
		}
	}
}

func TestClassifyUncertainCommandsAsk(t *testing.T) {
	commands := []string{
		"git add -p",
		"git add $TARGET",
		"git commit -a -m update",
		"git commit README.md",
		"cp oversized.bin attack.bin && git add attack.bin",
	}
	for _, command := range commands {
		if got := classifyCommand(command); got.Kind != actionAsk {
			t.Fatalf("%q: got %s, want ask", command, got.Kind)
		}
	}
}
