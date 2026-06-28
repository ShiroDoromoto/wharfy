package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/ShiroDoromoto/wharfy/internal/output"
	"github.com/ShiroDoromoto/wharfy/internal/registry"
)

// init は「2回目以降のリリースで agent が wharfy を素通りしない」ための一手。
// AGENTS.md / CLAUDE.md に「リリースは wharfy agent を実行して従え」という入口だけを
// 管理ブロックで書き込む。手順は焼き込まない——真実は常に `wharfy agent` 側にある(05 drift 対策)。

// initTargets は書き込む対象。単数 AGENT.md は事実上どのツールも読まないため複数形 AGENTS.md と
// CLAUDE.md の 2 本を狙う(Codex/Cursor 系 = AGENTS.md、Claude Code = CLAUDE.md)。
var initTargets = []string{"AGENTS.md", "CLAUDE.md"}

// 管理ブロックのマーカー。HTML コメントなのでレンダリング時は不可視。begin/end で囲うことで
// 2 回目以降は中身を差し替えるだけ(冪等)になり、追記の重複を防ぐ。
const (
	initBeginMarker = "<!-- wharfy:begin (managed) -->"
	initEndMarker   = "<!-- wharfy:end -->"
)

// managedBlock は焼き込む本文。手順ではなく「まず wharfy agent を実行せよ」という入口のみ。
func managedBlock() string {
	body := strings.Join([]string{
		"## Releasing",
		"",
		"Release and distribution for this project go through **wharfy**.",
		"Don't guess the steps — run `wharfy agent` first (agents: `wharfy agent --json`)",
		"and follow its output. That capability map is always current.",
		"",
		"Merge is not distribution. Auto-merging dependency bumps (Dependabot etc.) is fine,",
		"but **never auto-distribute**: distribution is an explicit, human/AI-gated step",
		"(`wharfy release` / `wharfy publish`). Let bumps accumulate, then ship deliberately.",
		"Do not wire CI to run release/publish unattended.",
	}, "\n")
	return initBeginMarker + "\n" + body + "\n" + initEndMarker
}

// stdinIsTTY / promptConfirm は対話確認の口。テストから差し替えられるよう var にする
// (auth.go の promptSecret と同じ思想)。
var stdinIsTTY = func() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

var promptConfirm = func(prompt string) (bool, error) {
	fmt.Fprint(os.Stderr, prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && err != io.EOF {
		return false, err
	}
	s := strings.ToLower(strings.TrimSpace(line))
	return s == "y" || s == "yes", nil
}

// filePlan は 1 ファイルの変更計画。content は書き込む全文(action==unchanged のとき未使用)。
type filePlan struct {
	Path    string `json:"path"`
	Action  string `json:"action"` // create | append | update | unchanged
	content string
}

// planFile は既存内容から「どう書き換えるか」を決める。副作用なし(計画のみ)。
//   - ファイル無し            → create
//   - 管理ブロック有り・同一  → unchanged
//   - 管理ブロック有り・差分  → update(ブロックだけ差し替え、前後は保つ)
//   - 管理ブロック無し        → append(末尾に 1 行空けて追記)
func planFile(existing string, exists bool) (content, action string) {
	block := managedBlock()
	if !exists {
		return block + "\n", "create"
	}
	if bi := strings.Index(existing, initBeginMarker); bi >= 0 {
		if rel := strings.Index(existing[bi:], initEndMarker); rel >= 0 {
			ei := bi + rel + len(initEndMarker) // end マーカー直後
			updated := existing[:bi] + block + existing[ei:]
			if updated == existing {
				return existing, "unchanged"
			}
			return updated, "update"
		}
		// begin だけで end が無い壊れた状態。安全側に倒して末尾追記で修復する。
	}
	base := strings.TrimRight(existing, "\n")
	return base + "\n\n" + block + "\n", "append"
}

// agentInstructionsPresent は cwd の AGENTS.md / CLAUDE.md のどれかに管理ブロックがあるかを返す。
// 「既存ユーザーが wharfy を使っているのに init していない」を status から気づかせるための判定。
// 読めないファイルはそのファイル単位で未検出扱い(誤検知でうるさく促さない安全側)。
func agentInstructionsPresent() bool {
	for _, name := range initTargets {
		data, err := os.ReadFile(name)
		if err != nil {
			continue
		}
		if strings.Contains(string(data), initBeginMarker) {
			return true
		}
	}
	return false
}

// withInitNudge は init 未実施なら成功 Result に「次は wharfy init」を一手足す。
// リリースを通した直後(release / publish 一括成功)こそ「次回はエージェントに wharfy で
// やらせたい」と気づく価値が高い。init 済みなら何も足さない(冪等・自己沈静)。
func withInitNudge(res output.Result) output.Result {
	if agentInstructionsPresent() {
		return res
	}
	res.Warnings = append(res.Warnings, output.Warning{
		Code:    output.WarnInitMissing,
		Message: "agents aren't told to release via wharfy yet; run `wharfy init` so the next release goes through wharfy",
	})
	res.Next = append(res.Next, output.NextDo{
		Reason: "tell agents to release via wharfy (so they don't reinvent it next time)",
		Do:     "wharfy init --yes",
	})
	return res
}

// runInit は AGENTS.md / CLAUDE.md に管理ブロックを書く。
// 書き込みは --yes で確定。--yes が無い場合は、TTY なら一度だけ対話確認、
// 非 TTY/--json/--dry-run ではプレビュー(何も書かない)に倒す——publish と同じ --yes ゲート思想。
func runInit(_ context.Context, c registry.Command, _ []string) output.Result {
	plans := make([]filePlan, 0, len(initTargets))
	for _, name := range initTargets {
		data, err := os.ReadFile(name)
		exists := err == nil
		if err != nil && !os.IsNotExist(err) {
			res := output.New(c.Name, "could not read "+name, false)
			res.Errors = []output.Problem{{Code: output.ErrInitWriteFailed, Message: err.Error(), Hint: "check file permissions in this directory and retry"}}
			res.Next = nextFromSpec(c)
			return res
		}
		content, action := planFile(string(data), exists)
		plans = append(plans, filePlan{Path: name, Action: action, content: content})
	}

	pending := 0
	for _, p := range plans {
		if p.Action != "unchanged" {
			pending++
		}
	}

	// 既に整っている。何もしない。
	if pending == 0 {
		res := output.New(c.Name, "agent instructions already point releases at wharfy ("+strings.Join(initTargets, ", ")+")", true)
		res.Data = initData(false, plans)
		res.Next = nextFromSpec(c)
		return res
	}

	// プレビューに倒す条件: --dry-run、または --yes 無しで対話できない(非 TTY / --json)。
	preview := flagDryRun || (!flagYes && (flagJSON || !stdinIsTTY()))
	if preview {
		printPlan(os.Stderr, plans)
		res := output.New(c.Name, fmt.Sprintf("preview: %d file(s) would change; re-run with --yes to apply", pending), true)
		res.Data = initData(false, plans)
		res.Next = append([]output.NextDo{{Reason: "write the agent instructions", Do: "wharfy init --yes"}}, nextFromSpec(c)...)
		return res
	}

	// --yes 無しの TTY: 一度だけ確認する。
	if !flagYes {
		printPlan(os.Stderr, plans)
		ok, err := promptConfirm("Apply these changes? [y/N]: ")
		if err != nil {
			res := output.New(c.Name, "could not read confirmation", false)
			res.Errors = []output.Problem{{Code: output.ErrInitWriteFailed, Message: err.Error(), Hint: "re-run with --yes to skip the prompt"}}
			res.Next = nextFromSpec(c)
			return res
		}
		if !ok {
			res := output.New(c.Name, "aborted; nothing written", true)
			res.Data = initData(false, plans)
			res.Next = append([]output.NextDo{{Reason: "apply without the prompt", Do: "wharfy init --yes"}}, nextFromSpec(c)...)
			return res
		}
	}

	// 確定して書き込む。
	for _, p := range plans {
		if p.Action == "unchanged" {
			continue
		}
		if err := os.WriteFile(p.Path, []byte(p.content), 0o644); err != nil {
			res := output.New(c.Name, "could not write "+p.Path, false)
			res.Errors = []output.Problem{{Code: output.ErrInitWriteFailed, Message: err.Error(), Hint: "check file permissions in this directory and retry"}}
			res.Next = nextFromSpec(c)
			return res
		}
	}

	res := output.New(c.Name, fmt.Sprintf("wrote agent instructions to %d file(s); releases now route through wharfy", pending), true)
	res.Data = initData(true, plans)
	res.Next = nextFromSpec(c)
	return res
}

// initData は Result.Data の体裁。applied は実際に書いたか(プレビュー時 false)。
func initData(applied bool, plans []filePlan) map[string]any {
	files := make([]map[string]string, 0, len(plans))
	for _, p := range plans {
		files = append(files, map[string]string{"path": p.Path, "action": p.Action})
	}
	return map[string]any{"applied": applied, "files": files}
}

// printPlan は変更計画を人間向けに stderr へ出す(対話確認・プレビュー共通)。
func printPlan(w io.Writer, plans []filePlan) {
	fmt.Fprintln(w, "wharfy init — planned changes:")
	for _, p := range plans {
		note := ""
		switch p.Action {
		case "append":
			note = " (append to existing file)"
		case "update":
			note = " (refresh managed block)"
		case "unchanged":
			note = " (already up to date)"
		}
		fmt.Fprintf(w, "  %-12s %s%s\n", p.Action, p.Path, note)
	}
}
