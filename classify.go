package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

type actionKind string

const (
	actionIgnore   actionKind = "ignore"
	actionStaged   actionKind = "staged"
	actionWorktree actionKind = "worktree"
	actionAsk      actionKind = "ask"
	actionDeny     actionKind = "deny"
)

type commandAction struct {
	Kind   actionKind
	Paths  []string
	Reason string
}

type HookInput struct {
	CWD       string `json:"cwd"`
	ToolName  string `json:"tool_name"`
	ToolInput struct {
		Command string `json:"command"`
	} `json:"tool_input"`
}

type HookDecision struct {
	HookSpecificOutput struct {
		HookEventName            string `json:"hookEventName"`
		PermissionDecision       string `json:"permissionDecision"`
		PermissionDecisionReason string `json:"permissionDecisionReason"`
	} `json:"hookSpecificOutput"`
}

func runHook(input io.Reader, output io.Writer, t thresholds) error {
	var event HookInput
	if err := json.NewDecoder(input).Decode(&event); err != nil {
		return fmt.Errorf("decode hook input: %w", err)
	}
	if event.ToolName != "Bash" {
		return nil
	}

	action := classifyCommand(event.ToolInput.Command)
	switch action.Kind {
	case actionIgnore:
		return nil
	case actionAsk, actionDeny:
		return writeHookDecision(output, string(action.Kind), action.Reason)
	}

	var report Report
	var err error
	if action.Kind == actionStaged {
		report, err = analyzeStaged(event.CWD, t)
	} else {
		report, err = analyzeWorktree(event.CWD, action.Paths, t)
	}
	if err != nil {
		return writeHookDecision(output, "ask", "The guard could not verify repository growth: "+err.Error())
	}
	if report.Decision == "allow" {
		return nil
	}

	reason := fmt.Sprintf("%s Projected new Git blob bytes: %d.", report.Reason, report.ProjectedNewGitBlobBytes)
	if len(report.Recommendations) > 0 {
		reason += " " + strings.Join(report.Recommendations, " ")
	}
	return writeHookDecision(output, report.Decision, reason)
}

func writeHookDecision(output io.Writer, decision, reason string) error {
	var result HookDecision
	result.HookSpecificOutput.HookEventName = "PreToolUse"
	result.HookSpecificOutput.PermissionDecision = decision
	result.HookSpecificOutput.PermissionDecisionReason = reason
	return json.NewEncoder(output).Encode(result)
}

func classifyCommand(command string) commandAction {
	segments, operators, err := tokenizeSegments(command)
	if err != nil {
		if looksGuarded(command) {
			return commandAction{Kind: actionAsk, Reason: "Dynamic or malformed shell syntax could not be verified."}
		}
		return commandAction{Kind: actionIgnore}
	}
	if len(segments) == 0 {
		return commandAction{Kind: actionIgnore}
	}
	if len(segments) > 1 || len(operators) > 0 {
		for _, segment := range segments {
			if guardedSegment(segment) {
				return commandAction{Kind: actionAsk, Reason: "Compound commands require confirmation until each segment is classified."}
			}
		}
		return commandAction{Kind: actionIgnore}
	}
	return classifySimple(segments[0])
}

func classifySimple(tokens []string) commandAction {
	if len(tokens) < 2 || tokens[0] != "git" {
		return commandAction{Kind: actionIgnore}
	}
	switch tokens[1] {
	case "add":
		return classifyAdd(tokens[2:])
	case "commit":
		return classifyCommit(tokens[2:])
	default:
		return commandAction{Kind: actionIgnore}
	}
}

func classifyAdd(args []string) commandAction {
	all := false
	afterDash := false
	paths := []string{}
	for _, arg := range args {
		if !afterDash && arg == "--" {
			afterDash = true
			continue
		}
		if !afterDash && (arg == "-A" || arg == "--all") {
			all = true
			continue
		}
		if !afterDash && strings.HasPrefix(arg, "-") {
			return commandAction{Kind: actionAsk, Reason: "Unsupported git add flag requires confirmation: " + arg}
		}
		if strings.ContainsAny(arg, "*?[") {
			return commandAction{Kind: actionAsk, Reason: "Glob pathspecs are not expanded by the guard."}
		}
		paths = append(paths, arg)
	}
	if !all && len(paths) == 0 {
		return commandAction{Kind: actionAsk, Reason: "git add without a supported literal pathspec is not verified."}
	}
	if all && len(paths) == 0 {
		paths = nil
	}
	return commandAction{Kind: actionWorktree, Paths: paths}
}

func classifyCommit(args []string) commandAction {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-m" || args[i] == "--message":
			if i+1 >= len(args) {
				return commandAction{Kind: actionAsk, Reason: "Commit message flag is missing its value."}
			}
			i++
		case strings.HasPrefix(args[i], "--message="):
		case args[i] == "--no-verify", args[i] == "--allow-empty", args[i] == "--allow-empty-message", args[i] == "-q", args[i] == "--quiet":
		case args[i] == "-a", args[i] == "--all", args[i] == "-i", args[i] == "--include", args[i] == "-o", args[i] == "--only", args[i] == "-p", args[i] == "--patch", args[i] == "--amend":
			return commandAction{Kind: actionAsk, Reason: "This git commit form can select or stage worktree content and requires confirmation."}
		default:
			return commandAction{Kind: actionAsk, Reason: "Unsupported git commit argument requires confirmation: " + args[i]}
		}
	}
	return commandAction{Kind: actionStaged}
}

func tokenizeSegments(command string) ([][]string, []string, error) {
	var segments [][]string
	var operators []string
	var tokens []string
	var word strings.Builder
	var quote byte

	flushWord := func() {
		if word.Len() > 0 {
			tokens = append(tokens, word.String())
			word.Reset()
		}
	}
	flushSegment := func(operator string) error {
		flushWord()
		if len(tokens) == 0 {
			return fmt.Errorf("empty command segment")
		}
		segments = append(segments, tokens)
		tokens = nil
		operators = append(operators, operator)
		return nil
	}

	for i := 0; i < len(command); i++ {
		ch := command[i]
		if quote != 0 {
			if ch == quote {
				quote = 0
				continue
			}
			if quote == '"' && (ch == '$' || ch == '`') {
				return nil, nil, fmt.Errorf("dynamic expansion")
			}
			if ch == '\\' && quote == '"' && i+1 < len(command) {
				i++
				word.WriteByte(command[i])
				continue
			}
			word.WriteByte(ch)
			continue
		}

		switch ch {
		case '\'', '"':
			quote = ch
		case '\\':
			if i+1 >= len(command) {
				return nil, nil, fmt.Errorf("trailing escape")
			}
			i++
			word.WriteByte(command[i])
		case ' ', '\t', '\r':
			flushWord()
		case '\n', ';', '', '':
			if err := flushSegment(";"); err != nil {
				return nil, nil, err
			}
		case '&':
			if i+1 >= len(command) || command[i+1] != '&' {
				return nil, nil, fmt.Errorf("background commands are unsupported")
			}
			i++
			if err := flushSegment("&&"); err != nil {
				return nil, nil, err
			}
		case '|':
			op := "|"
			if i+1 < len(command) && command[i+1] == '|' {
				i++
				op = "||"
			}
			if err := flushSegment(op); err != nil {
				return nil, nil, err
			}
		case '<', '>':
			if err := flushSegment(string(ch)); err != nil {
				return nil, nil, err
			}
		case '$', '`', '(', ')':
			return nil, nil, fmt.Errorf("dynamic shell syntax")
		default:
			word.WriteByte(ch)
		}
	}
	if quote != 0 {
		return nil, nil, fmt.Errorf("unterminated quote")
	}
	flushWord()
	if len(tokens) > 0 {
		segments = append(segments, tokens)
	}
	return segments, operators, nil
}

func guardedSegment(tokens []string) bool {
	if len(tokens) < 2 || tokens[0] != "git" {
		return false
	}
	for _, token := range tokens[1:] {
		if token == "add" || token == "commit" {
			return true
		}
	}
	return false
}

func looksGuarded(command string) bool {
	lower := strings.ToLower(command)
	return strings.Contains(lower, "git add") || strings.Contains(lower, "git commit")
}
