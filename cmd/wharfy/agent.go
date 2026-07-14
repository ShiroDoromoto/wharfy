package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/ShiroDoromoto/wharfy/internal/output"
	"github.com/ShiroDoromoto/wharfy/internal/registry"
)

// runAgent は ①「聞けば分かる」の一枚出力。registry から生成するので実体とズレない。
// --json は schemas/agent.json に valid な AgentDoc を出す。
func runAgent(asJSON bool) error {
	doc := registry.BuildAgentDoc(versionLine())
	if asJSON {
		s, err := output.Marshal(doc)
		if err != nil {
			return err
		}
		fmt.Fprint(os.Stdout, s)
		return nil
	}
	printAgentHuman(os.Stdout, doc)
	return nil
}

// noteWidth は注記の折り返し幅。端末に任せると継続行が行頭に戻り、どこまでが 1 つの注記か
// 見分けられなくなる——ぶら下げインデントで「1 注記 = 1 かたまり」を保つ。
const noteWidth = 96

// printAgentHuman は人間向けの体裁。
func printAgentHuman(w io.Writer, doc registry.AgentDoc) {
	fmt.Fprintln(w, "wharfy — ship one binary to every channel. Read this once, then drive.")
	fmt.Fprintf(w, "version: %s\n", doc.Version)
	fmt.Fprintln(w, "\nCOMMANDS (usual order)")
	for _, c := range doc.Commands {
		name := c.Name
		if c.Args != "" {
			name += " " + c.Args
		}
		line := fmt.Sprintf("  wharfy %-18s %s", name, c.Summary)
		if len(c.Next) > 0 {
			line += "   → next: " + strings.Join(c.Next, " | ")
		}
		fmt.Fprintln(w, line)
	}
	if len(doc.Channels) > 0 {
		names := make([]string, 0, len(doc.Channels))
		for _, ch := range doc.Channels {
			names = append(names, fmt.Sprintf("%s(%s)", ch.Name, ch.Kind))
		}
		fmt.Fprintf(w, "\nCHANNELS  %s\n", strings.Join(names, " "))
	}
	// 注記は「知らずに叩くと壊れる前提」なので、駆動する前に読ませる。--json にしか無かった頃は、
	// テキストで読む人だけがそれを知らないまま release を叩けた。コマンドもチャネルも同じ節に出す。
	printAgentNotes(w, doc)
	fmt.Fprintln(w, "\nEvery command takes --json and ends with a next: block.")
	fmt.Fprintf(w, "START HERE\n  %s\n", doc.Start)
}

// printAgentNotes はコマンド・チャネルの注記を 1 節にまとめて出す(注記が無ければ節ごと出さない)。
func printAgentNotes(w io.Writer, doc registry.AgentDoc) {
	type labeled struct {
		label string
		note  string
	}
	notes := make([]labeled, 0, 8)
	for _, c := range doc.Commands {
		for _, n := range c.Notes {
			notes = append(notes, labeled{c.Name, n})
		}
	}
	for _, ch := range doc.Channels {
		for _, n := range ch.Notes {
			notes = append(notes, labeled{ch.Name, n})
		}
	}
	if len(notes) == 0 {
		return
	}
	fmt.Fprintln(w, "\nNOTES (read before you drive)")
	for _, n := range notes {
		for i, line := range wrapNote(n.label+": "+n.note, noteWidth) {
			indent := "  "
			if i > 0 {
				indent = "    " // ぶら下げ: 継続行は一段深く
			}
			fmt.Fprintln(w, indent+line)
		}
	}
}

// wrapNote は語境界で折り返す(width より長い 1 語はそのまま出す——切ればコマンドや URL が壊れる)。
// 幅は rune 数で数える。注記には — や → が入るので、バイト数で数えると行が不揃いに短くなる。
func wrapNote(s string, width int) []string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return nil
	}
	lines := make([]string, 0, 4)
	line := words[0]
	for _, word := range words[1:] {
		if utf8.RuneCountInString(line)+1+utf8.RuneCountInString(word) > width {
			lines = append(lines, line)
			line = word
			continue
		}
		line += " " + word
	}
	return append(lines, line)
}
