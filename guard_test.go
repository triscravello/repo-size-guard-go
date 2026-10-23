package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestAlreadyTrackedContentNotCounted(t *testing.T) {
	repo := newRepo(t)
	writeTestFile(t, repo, "tracked.bin", bytes.Repeat([]byte{0}, 4096))
	mustGit(t, repo, "add", "tracked.bin")
	mustGit(t, repo, "commit", "-m", "track fixture")

	report, err := analyzeStaged(repo, thresholds{Warn: 1024, Fail: 2048})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Files) != 0 || report.ProjectedNewGitBlobBytes != 0 || report.Decision != "allow" {
		t.Fatalf("clean tracked content counted as growth: %+v", report)
	}
}

func TestOversizedBinaryDenied(t *testing.T) {
	repo := newRepo(t)
	writeTestFile(t, repo, "large.bin", bytes.Repeat([]byte{0}, 3072))
	mustGit(t, repo, "add", "large.bin")

	report, err := analyzeStaged(repo, thresholds{Warn: 1024, Fail: 2048})
	if err != nil {
		t.Fatal(err)
	}
	if report.Decision != "deny" || len(report.Files) != 1 || !report.Files[0].Binary {
		t.Fatalf("oversized binary was not denied: %+v", report)
	}
}

func TestLFSPointerSeparatesRawAndBlobSize(t *testing.T) {
	repo := newRepo(t)
	pointer := "version https://git-lfs.github.com/spec/v1\n" +
		"oid sha256:4d7a214614ab2935c943f9e0ff69d22eadbb8f32b1258daaa5e2ca24d17e2393\n" +
		"size 104857600\n"
	writeTestFile(t, repo, "asset.bin", []byte(pointer))
	mustGit(t, repo, "add", "asset.bin")
	if err := os.Truncate(filepath.Join(repo, "asset.bin"), 100*1024*1024); err != nil {
		t.Fatal(err)
	}

	report, err := analyzeStaged(repo, thresholds{Warn: defaultWarnBytes, Fail: defaultFailBytes})
	if err != nil {
		t.Fatal(err)
	}
	finding := report.Files[0]
	if !finding.LFSPointer || finding.RawBytes != 100*1024*1024 || finding.GitBlobBytes == nil || *finding.GitBlobBytes >= 1024 {
		t.Fatalf("raw and LFS pointer sizes were not separated: %+v", finding)
	}
	if report.Decision != "allow" {
		t.Fatalf("small staged pointer should be allowed: %+v", report)
	}
}

func TestWorktreeLFSFilterAsks(t *testing.T) {
	repo := newRepo(t)
	writeTestFile(t, repo, ".gitattributes", []byte("*.bin filter=lfs diff=lfs merge=lfs -text\n"))
	writeTestFile(t, repo, "asset.bin", bytes.Repeat([]byte{0}, 3072))

	report, err := analyzeWorktree(repo, []string{"asset.bin"}, thresholds{Warn: 1024, Fail: 2048})
	if err != nil {
		t.Fatal(err)
	}
	if report.Decision != "ask" || report.Files[0].ProjectedNewBlobBytes != nil {
		t.Fatalf("unstaged LFS clean output must remain uncertain: %+v", report)
	}
}

func TestHookOutcomes(t *testing.T) {
	cases := []struct {
		name     string
		size     int
		decision string
	}{
		{name: "safe", size: 512, decision: ""},
		{name: "warning", size: 1536, decision: "ask"},
		{name: "failure", size: 3072, decision: "deny"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newRepo(t)
			writeTestFile(t, repo, "candidate.bin", bytes.Repeat([]byte{0}, tc.size))
			mustGit(t, repo, "add", "candidate.bin")

			var event HookInput
			event.CWD = repo
			event.ToolName = "Bash"
			event.ToolInput.Command = "git commit -m \"fixture\""
			payload, err := json.Marshal(event)
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			if err := runHook(bytes.NewReader(payload), &output, thresholds{Warn: 1024, Fail: 2048}); err != nil {
				t.Fatal(err)
			}
			if tc.decision == "" {
				if output.Len() != 0 {
					t.Fatalf("safe hook printed a decision: %s", output.String())
				}
				return
			}
			var result HookDecision
			if err := json.Unmarshal(output.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.HookSpecificOutput.PermissionDecision != tc.decision {
				t.Fatalf("got %q, want %q", result.HookSpecificOutput.PermissionDecision, tc.decision)
			}
		})
	}
}

func BenchmarkParseStatus1000(b *testing.B) {
	var fixture bytes.Buffer
	for i := 0; i < 1000; i++ {
		fmt.Fprintf(&fixture, "?? file-%04d.bin%c", i, 0)
	}
	payload := fixture.Bytes()
	b.ReportAllocs()
	for b.Loop() {
		entries, err := parseStatus(payload)
		if err != nil || len(entries) != 1000 {
			b.Fatalf("parse failed: %v", err)
		}
	}
}

func BenchmarkClassifyCommand(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		action := classifyCommand("git add -- assets/video.bin docs/report.pdf")
		if action.Kind != actionWorktree {
			b.Fatal("unexpected classification")
		}
	}
}

func newRepo(t testing.TB) string {
	t.Helper()
	repo := t.TempDir()
	mustGit(t, repo, "init", "-b", "main")
	return repo
}

func writeTestFile(t testing.TB, repo, name string, content []byte) {
	t.Helper()
	path := filepath.Join(repo, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}
}

func mustGit(t testing.TB, repo string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Repo Guard Test",
		"GIT_AUTHOR_EMAIL=repo-guard@example.invalid",
		"GIT_COMMITTER_NAME=Repo Guard Test",
		"GIT_COMMITTER_EMAIL=repo-guard@example.invalid",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}
