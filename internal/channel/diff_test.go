package channel

import (
	"strings"
	"testing"
)

func TestDiffCreate(t *testing.T) {
	d := Diff("", "a\nb\n")
	if d != "+a\n+b\n" {
		t.Errorf("create diff = %q, want all-added", d)
	}
}

func TestDiffNoop(t *testing.T) {
	if d := Diff("x\ny\n", "x\ny\n"); d != "" {
		t.Errorf("identical → empty diff, got %q", d)
	}
}

func TestDiffChangedLine(t *testing.T) {
	d := Diff("a\nb\nc\n", "a\nB\nc\n")
	// 共通行は context、変更行は -/+。
	if !strings.Contains(d, " a\n") || !strings.Contains(d, "-b\n") || !strings.Contains(d, "+B\n") || !strings.Contains(d, " c\n") {
		t.Errorf("unexpected diff:\n%s", d)
	}
}

func TestDiffAddRemoveTail(t *testing.T) {
	d := Diff("a\n", "a\nb\nc\n")
	if !strings.Contains(d, " a\n") || !strings.Contains(d, "+b\n") || !strings.Contains(d, "+c\n") {
		t.Errorf("tail additions wrong:\n%s", d)
	}
}
