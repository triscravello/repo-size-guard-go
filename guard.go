package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type FileFinding struct {
	Path                  string `json:"path"`
	IndexStatus           string `json:"index_status"`
	RawBytes              int64  `json:"raw_bytes"`
	GitBlobBytes          *int64 `json:"git_blob_bytes,omitempty"`
	ProjectedNewBlobBytes *int64 `json:"projected_new_blob_bytes,omitempty"`
	Measurement           string `json:"measurement"`
	MeasurementExact      bool   `json:"measurement_exact"`
	AlreadyTracked        bool   `json:"already_tracked"`
	Binary                bool   `json:"binary"`
	LFSPointer            bool   `json:"lfs_pointer"`
	LFSObjectBytes        *int64 `json:"lfs_object_bytes,omitempty"`
	Note                  string `json:"note"`
}

type Report struct {
	Scope                        string        `json:"scope"`
	Thresholds                   thresholds    `json:"thresholds"`
	TotalRawBytes                int64         `json:"total_raw_bytes"`
	ProjectedNewGitBlobBytes     int64         `json:"projected_new_git_blob_bytes"`
	LogicalBlobMeasurementsExact bool          `json:"logical_blob_measurements_exact"`
	ProjectedStorageIsEstimate   bool          `json:"projected_storage_is_estimate"`
	Decision                     string        `json:"decision"`
	Reason                       string        `json:"reason"`
	Files                        []FileFinding `json:"files"`
	Uncertainties                []string      `json:"uncertainties,omitempty"`
	Recommendations              []string      `json:"recommendations,omitempty"`
}

type statusEntry struct {
	X    byte
	Y    byte
	Path string
}

func analyzeStaged(repo string, t thresholds) (Report, error) {
	report := Report{
		Scope:                        "staged",
		Thresholds:                   t,
		LogicalBlobMeasurementsExact: true,
		ProjectedStorageIsEstimate:   true,
		Files:                        []FileFinding{},
	}
	entries, err := readStatus(repo, nil)
	if err != nil {
		return report, err
	}

	for _, entry := range entries {
		if unmerged(entry) {
			report.Uncertainties = append(report.Uncertainties, "unmerged index entry: "+entry.Path)
			continue
		}
		if entry.X != 'A' && entry.X != 'M' {
			continue
		}
		finding, err := stagedFinding(repo, entry)
		if err != nil {
			return report, err
		}
		report.Files = append(report.Files, finding)
	}
	finalizeReport(&report)
	return report, nil
}

func analyzeWorktree(repo string, paths []string, t thresholds) (Report, error) {
	report := Report{
		Scope:                        "worktree",
		Thresholds:                   t,
		LogicalBlobMeasurementsExact: false,
		ProjectedStorageIsEstimate:   true,
		Files:                        []FileFinding{},
	}
	entries, err := readStatus(repo, paths)
	if err != nil {
		return report, err
	}

	for _, entry := range entries {
		if unmerged(entry) {
			report.Uncertainties = append(report.Uncertainties, "unmerged worktree entry: "+entry.Path)
			continue
		}
		untracked := entry.X == '?' && entry.Y == '?'
		if !untracked && entry.Y != 'M' && entry.Y != 'A' && entry.Y != 'T' {
			continue
		}

		fullPath := filepath.Join(repo, filepath.FromSlash(entry.Path))
		info, err := os.Lstat(fullPath)
		if err != nil {
			return report, fmt.Errorf("stat %s: %w", entry.Path, err)
		}
		finding := FileFinding{
			Path:             entry.Path,
			IndexStatus:      string([]byte{entry.X, entry.Y}),
			RawBytes:         info.Size(),
			AlreadyTracked:   !untracked,
			Measurement:      "raw-file-estimate",
			MeasurementExact: false,
			Note:             "The worktree size is a conservative estimate until Git writes the cleaned blob to the index.",
		}
		if !info.Mode().IsRegular() {
			report.Uncertainties = append(report.Uncertainties, "non-regular path requires confirmation: "+entry.Path)
			finding.Measurement = "unsupported-file-type"
			report.Files = append(report.Files, finding)
			continue
		}

		filter, err := filterAttribute(repo, entry.Path)
		if err != nil {
			return report, err
		}
		if filter == "lfs" {
			finding.Measurement = "lfs-clean-output-unknown"
			finding.Note = "Git LFS is configured for this path, so stage it first to measure the pointer blob exactly."
			report.Uncertainties = append(report.Uncertainties, "Git LFS clean output is not staged yet: "+entry.Path)
			report.Files = append(report.Files, finding)
			continue
		}
		if filter != "unspecified" && filter != "unset" {
			finding.Measurement = "custom-filter-output-unknown"
			finding.Note = "A custom clean filter can change the blob, so the operation requires confirmation."
			report.Uncertainties = append(report.Uncertainties, "custom filter output is unknown for "+entry.Path)
			report.Files = append(report.Files, finding)
			continue
		}

		estimate := info.Size()
		finding.GitBlobBytes = &estimate
		finding.ProjectedNewBlobBytes = &estimate
		finding.Binary, err = sampleIsBinary(fullPath)
		if err != nil {
			return report, err
		}
		if finding.Binary && estimate >= t.Warn {
			appendUnique(&report.Recommendations, "Consider Git LFS or a release asset for large binary content.")
		}
		report.Files = append(report.Files, finding)
	}
	finalizeReport(&report)
	return report, nil
}

func stagedFinding(repo string, entry statusEntry) (FileFinding, error) {
	oid, err := stagedOID(repo, entry.Path)
	if err != nil {
		return FileFinding{}, err
	}
	sizeOutput, err := gitOutput(repo, "cat-file", "-s", oid)
	if err != nil {
		return FileFinding{}, err
	}
	size, err := strconv.ParseInt(strings.TrimSpace(string(sizeOutput)), 10, 64)
	if err != nil {
		return FileFinding{}, fmt.Errorf("parse blob size for %s: %w", entry.Path, err)
	}

	finding := FileFinding{
		Path:                  entry.Path,
		IndexStatus:           string([]byte{entry.X, entry.Y}),
		GitBlobBytes:          &size,
		ProjectedNewBlobBytes: &size,
		Measurement:           "exact-index-blob",
		MeasurementExact:      true,
		AlreadyTracked:        entry.X == 'M',
		Note:                  "The index blob size is exact. Physical packfile growth remains an estimate because Git may deduplicate, delta-compress, or compress objects.",
	}
	fullPath := filepath.Join(repo, filepath.FromSlash(entry.Path))
	if info, statErr := os.Stat(fullPath); statErr == nil {
		finding.RawBytes = info.Size()
		finding.Binary, err = sampleIsBinary(fullPath)
		if err != nil {
			return FileFinding{}, err
		}
	}

	if size < 1024 {
		content, contentErr := gitOutput(repo, "cat-file", "blob", oid)
		if contentErr != nil {
			return FileFinding{}, contentErr
		}
		if lfsSize, ok := parseLFSPointer(content); ok {
			finding.LFSPointer = true
			finding.LFSObjectBytes = &lfsSize
			finding.Note = "Git stores this exact small pointer blob. The large LFS object is stored separately and is not counted as Git blob growth."
		}
	}
	return finding, nil
}

func readStatus(repo string, paths []string) ([]statusEntry, error) {
	args := []string{"status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames"}
	if len(paths) > 0 {
		args = append(args, "--")
		args = append(args, paths...)
	}
	out, err := gitOutput(repo, args...)
	if err != nil {
		return nil, err
	}
	return parseStatus(out)
}

func parseStatus(out []byte) ([]statusEntry, error) {
	records := bytes.Split(out, []byte{0})
	entries := make([]statusEntry, 0, len(records))
	for _, record := range records {
		if len(record) == 0 {
			continue
		}
		if len(record) < 4 || record[2] != ' ' {
			return nil, fmt.Errorf("unexpected porcelain record %q", record)
		}
		entries = append(entries, statusEntry{X: record[0], Y: record[1], Path: string(record[3:])})
	}
	return entries, nil
}

func stagedOID(repo, path string) (string, error) {
	out, err := gitOutput(repo, "ls-files", "--stage", "-z", "--", path)
	if err != nil {
		return "", err
	}
	record := bytes.TrimSuffix(out, []byte{0})
	parts := bytes.SplitN(record, []byte{'\t'}, 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("missing stage-0 entry for %s", path)
	}
	fields := strings.Fields(string(parts[0]))
	if len(fields) != 3 || fields[2] != "0" {
		return "", fmt.Errorf("unexpected index entry for %s", path)
	}
	return fields[1], nil
}

func filterAttribute(repo, path string) (string, error) {
	input := append([]byte(path), 0)
	out, err := gitInputOutput(repo, input, "check-attr", "--stdin", "-z", "filter")
	if err != nil {
		return "", err
	}
	parts := bytes.Split(out, []byte{0})
	if len(parts) < 3 || string(parts[1]) != "filter" {
		return "", fmt.Errorf("unexpected check-attr output for %s", path)
	}
	return string(parts[2]), nil
}

func gitOutput(repo string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, err
	}
	return out, nil
}

func gitInputOutput(repo string, input []byte, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	cmd.Stdin = bytes.NewReader(input)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, err
	}
	return out, nil
}

func sampleIsBinary(path string) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()

	sample := make([]byte, 8192)
	n, err := file.Read(sample)
	if err != nil && err != io.EOF {
		return false, err
	}
	return bytes.IndexByte(sample[:n], 0) >= 0, nil
}

func parseLFSPointer(content []byte) (int64, bool) {
	if len(content) == 0 || len(content) >= 1024 {
		return 0, false
	}
	lines := strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
	if len(lines) < 3 || lines[0] != "version https://git-lfs.github.com/spec/v1" {
		return 0, false
	}
	var oid string
	var sizeText string
	for _, line := range lines[1:] {
		if strings.HasPrefix(line, "oid sha256:") {
			oid = strings.TrimPrefix(line, "oid sha256:")
		}
		if strings.HasPrefix(line, "size ") {
			sizeText = strings.TrimPrefix(line, "size ")
		}
	}
	if len(oid) != 64 || !lowerHex(oid) || sizeText == "" {
		return 0, false
	}
	size, err := strconv.ParseInt(sizeText, 10, 64)
	return size, err == nil && size >= 0
}

func lowerHex(value string) bool {
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func unmerged(entry statusEntry) bool {
	return entry.X == 'U' || entry.Y == 'U' || (entry.X == 'A' && entry.Y == 'A') || (entry.X == 'D' && entry.Y == 'D')
}

func finalizeReport(report *Report) {
	unknown := len(report.Uncertainties) > 0
	for _, file := range report.Files {
		report.TotalRawBytes += file.RawBytes
		if file.ProjectedNewBlobBytes == nil {
			unknown = true
			continue
		}
		report.ProjectedNewGitBlobBytes += *file.ProjectedNewBlobBytes
	}

	switch {
	case report.ProjectedNewGitBlobBytes >= report.Thresholds.Fail:
		report.Decision = "deny"
		report.Reason = "Projected Git blob growth meets or exceeds the failure threshold."
		appendUnique(&report.Recommendations, "Use Git LFS, a release asset, object storage, or remove the oversized content before retrying.")
	case unknown:
		report.Decision = "ask"
		report.Reason = "At least one operation or cleaned blob size is uncertain and requires confirmation."
	case report.ProjectedNewGitBlobBytes >= report.Thresholds.Warn:
		report.Decision = "ask"
		report.Reason = "Projected Git blob growth meets or exceeds the warning threshold."
		appendUnique(&report.Recommendations, "Confirm the growth intentionally or move large binary content to Git LFS or a release asset.")
	default:
		report.Decision = "allow"
		report.Reason = "All supported measurements are below the warning threshold."
	}
}

func appendUnique(values *[]string, value string) {
	for _, existing := range *values {
		if existing == value {
			return
		}
	}
	*values = append(*values, value)
}
