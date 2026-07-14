package main

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ShiroDoromoto/wharfy/internal/registry"
)

// 人間向けの一枚出力にも注記が載ること。注記は「知らずに叩くと壊れる前提」なので、--json で
// 読む agent だけが知っていて、テキストで読む人が知らない、という非対称は許さない。
func TestAgentHumanPrintsNotes(t *testing.T) {
	var buf bytes.Buffer
	printAgentHuman(&buf, registry.BuildAgentDoc("v0.0.0-test"))
	got := buf.String()

	if !strings.Contains(got, "NOTES") {
		t.Fatalf("human output has no NOTES section:\n%s", got)
	}
	// registry が持つ注記は、コマンドのものもチャネルのものも 1 つ残らず出す。
	for _, c := range registry.Commands {
		for _, n := range c.Notes {
			if !containsWrapped(got, c.Name+": "+n) {
				t.Errorf("command note not printed (%s): %q", c.Name, n)
			}
		}
	}
	for _, ch := range registry.Channels {
		for _, n := range ch.Notes {
			if !containsWrapped(got, ch.Name+": "+n) {
				t.Errorf("channel note not printed (%s): %q", ch.Name, n)
			}
		}
	}
}

// 折り返しても幅を超えず、語は割らない(URL やコマンドが途中で切れると貼れなくなる)。
func TestWrapNote(t *testing.T) {
	lines := wrapNote("release: re-running it on the same tag is safe and the assets are replaced", 20)
	for _, l := range lines {
		if utf8.RuneCountInString(l) > 20 && len(strings.Fields(l)) > 1 {
			t.Errorf("line exceeds width without being a single long word: %q", l)
		}
	}
	if joined := strings.Join(lines, " "); joined != "release: re-running it on the same tag is safe and the assets are replaced" {
		t.Errorf("wrapping changed the text: %q", joined)
	}

	// 幅より長い 1 語は切らずにそのまま出す。
	long := "https://example.com/a/very/long/path/that/exceeds/the/width/by/far"
	if lines := wrapNote(long, 20); len(lines) != 1 || lines[0] != long {
		t.Errorf("a long word must survive intact: %q", lines)
	}
}

// containsWrapped は「折り返して出力された注記」を、空白の潰し込みを許して探す。
func containsWrapped(out, note string) bool {
	flat := strings.Join(strings.Fields(out), " ")
	return strings.Contains(flat, strings.Join(strings.Fields(note), " "))
}
