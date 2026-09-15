package matcher

import (
	"strings"
	"testing"
)

func TestPipelineMatcher_TwoStageFixedToRegex(t *testing.T) {
	// -Fe 'error' -toe '\d+'
	stages := []StageConfig{
		{Pattern: "error", Fixed: true},
		{Pattern: `\d+`, OnlyMatch: true},
	}
	m, err := NewMatcherFromPipelines([][]StageConfig{stages}, false, false, MatcherOpts{})
	if err != nil {
		t.Fatal(err)
	}

	data := []byte("error: code 503\ninfo: ok\nerror: code 200\n")
	ms := m.FindAll(data)

	if ms.Len() != 2 {
		t.Fatalf("expected 2 matches, got %d", ms.Len())
	}

	// Check that positions point to the digits
	for i, want := range []string{"503", "200"} {
		positions := ms.MatchPositions(i)
		if len(positions) == 0 {
			t.Fatalf("match %d: no positions", i)
		}
		lineBytes := ms.LineBytes(i)
		pos := positions[0]
		got := string(lineBytes[pos[0]:pos[1]])
		if got != want {
			t.Errorf("match %d: got %q, want %q", i, got, want)
		}
	}
}

func TestPipelineMatcher_ThreeStageNarrowing(t *testing.T) {
	// -Fe 'HTTP' -te 'status=\d+' -toe '(?:status=)(\d+)'
	// Each stage filters at line level. The final stage extracts the status code.
	// Use a specific final regex to extract just the status code digits.
	stages := []StageConfig{
		{Pattern: "HTTP", Fixed: true},
		{Pattern: `status=\d+`},
		{Pattern: `status=(\d+)`, OnlyMatch: true},
	}
	m, err := NewMatcherFromPipelines([][]StageConfig{stages}, false, false, MatcherOpts{})
	if err != nil {
		t.Fatal(err)
	}

	data := []byte("HTTP/1.1 status=200 OK\nFTP transfer done\nHTTP/1.1 status=503 Error\n")
	ms := m.FindAll(data)

	if ms.Len() != 2 {
		t.Fatalf("expected 2 matches, got %d", ms.Len())
	}

	// The final regex `status=(\d+)` matches "status=200" and "status=503"
	for i, want := range []string{"status=200", "status=503"} {
		positions := ms.MatchPositions(i)
		if len(positions) == 0 {
			t.Fatalf("match %d: no positions", i)
		}
		lineBytes := ms.LineBytes(i)
		pos := positions[0]
		got := string(lineBytes[pos[0]:pos[1]])
		if got != want {
			t.Errorf("match %d: got %q, want %q", i, got, want)
		}
	}
}

func TestPipelineMatcher_ThreeStageFragmentExtraction(t *testing.T) {
	// Staged narrowing (report bug 5): each -t stage searches only the
	// previous stage's matched fragments, so the final \d+ must extract
	// "350" from "timeout=350" — never "127" from elsewhere on the line.
	// -e 'ERROR' -te 'timeout=\d+' -toe '\d+'
	stages := []StageConfig{
		{Pattern: "ERROR"},
		{Pattern: `timeout=\d+`},
		{Pattern: `\d+`, OnlyMatch: true},
	}
	m, err := NewMatcherFromPipelines([][]StageConfig{stages}, false, false, MatcherOpts{})
	if err != nil {
		t.Fatal(err)
	}

	data := []byte("ERROR conn reset 127.0.0.1 timeout=350ms\nINFO timeout=99ms\n")
	ms := m.FindAll(data)

	if ms.Len() != 1 {
		t.Fatalf("got %d matches, want 1 (INFO line filtered by stage 0)", ms.Len())
	}

	var fragments []string
	lineBytes := ms.LineBytes(0)
	for _, pos := range ms.MatchPositions(0) {
		fragments = append(fragments, string(lineBytes[pos[0]:pos[1]]))
	}
	if len(fragments) != 1 || fragments[0] != "350" {
		t.Errorf("narrowed fragments = %v, want [350]", fragments)
	}
}

func TestPipelineMatcher_IntermediateStageMustHitFragment(t *testing.T) {
	// A stage whose fragment contains no material for the next stage
	// kills the line: 'status' narrows to the literal text "status",
	// which has no digits.
	stages := []StageConfig{
		{Pattern: "HTTP", Fixed: true},
		{Pattern: `status`},
		{Pattern: `\d+`, OnlyMatch: true},
	}
	m, err := NewMatcherFromPipelines([][]StageConfig{stages}, false, false, MatcherOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if ms := m.FindAll([]byte("HTTP/1.1 status=200 OK\n")); ms.Len() != 0 {
		t.Errorf("got %d matches, want 0 (fragment %q has no digits)", ms.Len(), "status")
	}
}

func TestPipelineMatcher_NoMatchAtStage1(t *testing.T) {
	// -Fe 'error' -toe 'xyz'
	stages := []StageConfig{
		{Pattern: "error", Fixed: true},
		{Pattern: "xyz", OnlyMatch: true},
	}
	m, err := NewMatcherFromPipelines([][]StageConfig{stages}, false, false, MatcherOpts{})
	if err != nil {
		t.Fatal(err)
	}

	data := []byte("error: code 503\n")
	ms := m.FindAll(data)

	if ms.Len() != 0 {
		t.Errorf("expected 0 matches, got %d", ms.Len())
	}
}

func TestPipelineMatcher_MultipleMatchesPerLine(t *testing.T) {
	// -e 'x\d' -toe '\d'
	stages := []StageConfig{
		{Pattern: `x\d`},
		{Pattern: `\d`, OnlyMatch: true},
	}
	m, err := NewMatcherFromPipelines([][]StageConfig{stages}, false, false, MatcherOpts{})
	if err != nil {
		t.Fatal(err)
	}

	data := []byte("x1 x2 x3\n")
	ms := m.FindAll(data)

	if ms.Len() == 0 {
		t.Fatal("expected matches, got 0")
	}

	// Collect all matched digit positions
	var gotDigits []string
	for i := range ms.Matches {
		lineBytes := ms.LineBytes(i)
		for _, pos := range ms.MatchPositions(i) {
			gotDigits = append(gotDigits, string(lineBytes[pos[0]:pos[1]]))
		}
	}

	want := []string{"1", "2", "3"}
	if len(gotDigits) != len(want) {
		t.Fatalf("expected %d digits, got %d: %v", len(want), len(gotDigits), gotDigits)
	}
	for i := range want {
		if gotDigits[i] != want[i] {
			t.Errorf("digit %d: got %q, want %q", i, gotDigits[i], want[i])
		}
	}
}

func TestPipelineMatcher_EmptyMatchRegion(t *testing.T) {
	// -Fe 'x' -toe 'y'
	stages := []StageConfig{
		{Pattern: "x", Fixed: true},
		{Pattern: "y", OnlyMatch: true},
	}
	m, err := NewMatcherFromPipelines([][]StageConfig{stages}, false, false, MatcherOpts{})
	if err != nil {
		t.Fatal(err)
	}

	data := []byte("xaaa\n")
	ms := m.FindAll(data)

	if ms.Len() != 0 {
		t.Errorf("expected 0 matches, got %d", ms.Len())
	}
}

func TestPipelineMatcher_CaseInsensitive(t *testing.T) {
	// -i -Fe 'ERROR' -toe '\d+'
	stages := []StageConfig{
		{Pattern: "ERROR", Fixed: true},
		{Pattern: `\d+`, OnlyMatch: true},
	}
	m, err := NewMatcherFromPipelines([][]StageConfig{stages}, true, false, MatcherOpts{})
	if err != nil {
		t.Fatal(err)
	}

	data := []byte("Error: 503\nerror: 200\nERROR: 404\n")
	ms := m.FindAll(data)

	if ms.Len() != 3 {
		t.Fatalf("expected 3 matches, got %d", ms.Len())
	}
}

func TestPipelineMatcher_MatchExists(t *testing.T) {
	stages := []StageConfig{
		{Pattern: "error", Fixed: true},
		{Pattern: `\d+`, OnlyMatch: true},
	}
	m, err := NewMatcherFromPipelines([][]StageConfig{stages}, false, false, MatcherOpts{})
	if err != nil {
		t.Fatal(err)
	}

	// Has match
	if !m.MatchExists([]byte("error: code 503\n")) {
		t.Error("expected MatchExists=true")
	}

	// No match (no digits)
	if m.MatchExists([]byte("error: no code\n")) {
		t.Error("expected MatchExists=false for no digits")
	}

	// No match (no error)
	if m.MatchExists([]byte("info: code 503\n")) {
		t.Error("expected MatchExists=false for no error")
	}
}

func TestPipelineMatcher_CountAll(t *testing.T) {
	stages := []StageConfig{
		{Pattern: "error", Fixed: true},
		{Pattern: `\d+`, OnlyMatch: true},
	}
	m, err := NewMatcherFromPipelines([][]StageConfig{stages}, false, false, MatcherOpts{})
	if err != nil {
		t.Fatal(err)
	}

	data := []byte("error: 503\ninfo: ok\nerror: 200\nerror: no code\n")
	count := m.CountAll(data)
	if count != 2 {
		t.Errorf("expected count 2, got %d", count)
	}
}

func TestPipelineMatcher_SingleStage(t *testing.T) {
	// Single stage pipeline should work like regular matcher
	stages := []StageConfig{
		{Pattern: "error", Fixed: true},
	}
	m, err := NewMatcherFromPipelines([][]StageConfig{stages}, false, false, MatcherOpts{})
	if err != nil {
		t.Fatal(err)
	}

	data := []byte("error: code 503\ninfo: ok\n")
	ms := m.FindAll(data)
	if ms.Len() != 1 {
		t.Fatalf("expected 1 match, got %d", ms.Len())
	}
}

func TestPipelineMatcher_OnlyMatchSingleStage(t *testing.T) {
	// -oe '\d+' — only-matched without pipeline
	stages := []StageConfig{
		{Pattern: `\d+`, OnlyMatch: true},
	}
	m, err := NewMatcherFromPipelines([][]StageConfig{stages}, false, false, MatcherOpts{})
	if err != nil {
		t.Fatal(err)
	}

	// Verify it's a PipelineMatcher with onlyMatch
	pm, ok := m.(*PipelineMatcher)
	if !ok {
		t.Fatalf("expected PipelineMatcher, got %T", m)
	}
	if !pm.OnlyMatch() {
		t.Error("expected OnlyMatch=true")
	}
}

func TestPipelineMatcher_PipeWithoutPrevious(t *testing.T) {
	// First stage should not have Pipe=true — but this is validated at config level,
	// not in the matcher. Matcher just handles it gracefully.
	stages := []StageConfig{
		{Pattern: "foo", Fixed: true},
		{Pattern: `\d+`},
	}
	_, err := NewMatcherFromPipelines([][]StageConfig{stages}, false, false, MatcherOpts{})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPipelineMatcher_FindLine(t *testing.T) {
	stages := []StageConfig{
		{Pattern: "error", Fixed: true},
		{Pattern: `\d+`, OnlyMatch: true},
	}
	m, err := NewMatcherFromPipelines([][]StageConfig{stages}, false, false, MatcherOpts{})
	if err != nil {
		t.Fatal(err)
	}

	line := []byte("error: code 503 and 404")
	ms, ok := m.FindLine(line, 1, 0)
	if !ok {
		t.Fatal("expected match")
	}

	if ms.Len() != 1 {
		t.Fatalf("expected 1 match, got %d", ms.Len())
	}

	// Should find digits within the "error" match region
	positions := ms.MatchPositions(0)
	if len(positions) == 0 {
		t.Fatal("no positions")
	}
}

func TestMultiPipelineMatcher(t *testing.T) {
	// Pipeline 0: -Fe 'ERROR' -toe '\d+'
	// Pipeline 1: -Fe 'WARN'
	pipelines := [][]StageConfig{
		{
			{Pattern: "ERROR", Fixed: true},
			{Pattern: `\d+`, OnlyMatch: true},
		},
		{
			{Pattern: "WARN", Fixed: true},
		},
	}

	m, err := NewMatcherFromPipelines(pipelines, false, false, MatcherOpts{})
	if err != nil {
		t.Fatal(err)
	}

	data := []byte("ERROR: 503\nINFO: ok\nWARN: something\n")
	ms := m.FindAll(data)

	if ms.Len() < 2 {
		t.Fatalf("expected at least 2 matches, got %d", ms.Len())
	}
}

func TestMultiPipelineMatcher_MatchExists(t *testing.T) {
	pipelines := [][]StageConfig{
		{
			{Pattern: "ERROR", Fixed: true},
			{Pattern: `\d+`, OnlyMatch: true},
		},
		{
			{Pattern: "WARN", Fixed: true},
		},
	}

	m, err := NewMatcherFromPipelines(pipelines, false, false, MatcherOpts{})
	if err != nil {
		t.Fatal(err)
	}

	// Matches via pipeline 1
	if !m.MatchExists([]byte("WARN: something\n")) {
		t.Error("expected MatchExists=true for WARN")
	}

	// No match
	if m.MatchExists([]byte("INFO: ok\n")) {
		t.Error("expected MatchExists=false")
	}
}

func TestPipelineMatcher_LargeInput(t *testing.T) {
	// Sparse matches in large input to verify correctness at scale
	stages := []StageConfig{
		{Pattern: "TARGET", Fixed: true},
		{Pattern: `\d+`, OnlyMatch: true},
	}
	m, err := NewMatcherFromPipelines([][]StageConfig{stages}, false, false, MatcherOpts{})
	if err != nil {
		t.Fatal(err)
	}

	var sb strings.Builder
	for i := range 10000 {
		if i%1000 == 0 {
			sb.WriteString("TARGET: code 503\n")
		} else {
			sb.WriteString("info: nothing here\n")
		}
	}
	data := []byte(sb.String())

	ms := m.FindAll(data)
	if ms.Len() != 10 {
		t.Errorf("expected 10 matches, got %d", ms.Len())
	}

	if !m.MatchExists(data) {
		t.Error("MatchExists should be true")
	}

	if m.CountAll(data) != 10 {
		t.Errorf("CountAll expected 10, got %d", m.CountAll(data))
	}
}
