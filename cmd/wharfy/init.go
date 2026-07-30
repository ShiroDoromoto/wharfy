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
// 管理ブロックで書き込む。手順は焼き込まない——真実は常に `wharfy agent` 側にある(drift 対策)。

// initTargets は書き込む対象。単数 AGENT.md は事実上どのツールも読まないため複数形 AGENTS.md と
// CLAUDE.md の 2 本を狙う(Codex/Cursor 系 = AGENTS.md、Claude Code = CLAUDE.md)。
var initTargets = []string{"AGENTS.md", "CLAUDE.md"}

// 管理ブロックのマーカー。HTML コメントなのでレンダリング時は不可視。begin/end で囲うことで
// 2 回目以降は中身を差し替えるだけ(冪等)になり、追記の重複を防ぐ。
const (
	initBeginMarker = "<!-- wharfy:begin (managed) -->"
	initEndMarker   = "<!-- wharfy:end -->"
)

// managedBlock は焼き込む本文。入口だけを書き、中身は一切持たない。
//
// この本文は各プロジェクトの AGENTS.md / CLAUDE.md に撒かれる「wharfy の分身」で、wharfy が
// 変わっても追随しない。だから仕様・方針(非対話であること、引き金の作法)を一行でも焼き込めば、
// 撒かれた先で古い記述として残り、エージェントはそれを読む。内容を持たなければ陳腐化しようがない
// ——変わりうる話は全部 `wharfy agent`(常に現行版)側の注記で語る。
//
// 言うのは二言だけ: 「リリースはこのツールを通す」「できることは wharfy agent にある」。
// 禁止形・戒めの調子は書かない——読むのは指示に従うエージェントで、この本文に無い制約まで
// 方針として読み取ってしまう(現に「自動配布しない」と読まれ、CI を使うか使わないかという
// 利用者の判断にまで及んだ)。何をどう回すかは利用者が決める。ここはその入口を指すだけ。
func managedBlock() string {
	body := strings.Join([]string{
		"## Releasing",
		"",
		"Release and distribution for this project go through **wharfy**.",
		"What it can do is in `wharfy agent` (`wharfy agent --json`).",
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

// instructionsState は cwd の AGENTS.md / CLAUDE.md にある管理ブロックの現況。
//
// 「有る／無い」だけでは足りない。ブロックは各プロジェクトへ撒かれた写しなので、wharfy が本文を
// 変えれば撒かれた先は古いまま残り、エージェントは古い入口を読む——しかも誰も気づけない。
// だから「今の wharfy が書く本文と一致するか」まで見る(指紋は持たせない。本文そのものが指紋で、
// 版を埋め込めばそれ自体が毎版の差分ノイズになる)。
type instructionsState int

const (
	instructionsMissing instructionsState = iota // ブロックがどこにも無い(init 未実施)
	instructionsStale                            // ブロックはあるが、今の wharfy が書く本文と違う
	instructionsCurrent                          // 現行版のブロックがある
)

// agentInstructionsState は管理ブロックの現況を返す。判定は planFile に委ねる(書く側と読む側で
// 判定が二重化すればいずれずれる)。読めないファイルはそのファイル単位で見なかったことにする
// (誤検知でうるさく促さない安全側)。1 ファイルでも古ければ全体を stale と言う——そのファイルを
// 読むエージェントは現に古い入口を読むのだから、他方が新しくても救いにならない。
func agentInstructionsState() instructionsState {
	state := instructionsMissing
	for _, name := range initTargets {
		data, err := os.ReadFile(name)
		if err != nil {
			continue
		}
		switch _, action := planFile(string(data), true); action {
		case "update":
			return instructionsStale
		case "unchanged":
			state = instructionsCurrent
		}
	}
	return state
}

// initAdvice は管理ブロックの現況から「言うべき一手」を返す(ok=false なら言うことは無い)。
// status と withInitNudge が同じ語り口で促すための単一の口。
//
// 見つけても黙って書き換えはしない。管理ブロックが載るのは利用者のファイル(AGENTS.md /
// CLAUDE.md)で、release や status の副作用でそこが書き換わるのは驚きが大きすぎる。
// 書くのは init だけ、という境界は保ったまま、気づけなかった陳腐化を言葉にする。
func initAdvice() (output.Warning, output.NextDo, bool) {
	switch agentInstructionsState() {
	case instructionsMissing:
		return output.Warning{
				Code:    output.WarnInitMissing,
				Message: "agents aren't told to release via wharfy yet (no AGENTS.md/CLAUDE.md block); run `wharfy init`",
			}, output.NextDo{
				Reason: "tell agents to release via wharfy (so they don't reinvent it next time)",
				Do:     "wharfy init --yes",
			}, true
	case instructionsStale:
		return output.Warning{
				Code:    output.WarnInitStale,
				Message: "the wharfy block in AGENTS.md/CLAUDE.md was written by an older wharfy and no longer matches what this one writes; run `wharfy init` to refresh it",
			}, output.NextDo{
				Reason: "refresh the managed block agents read (it is a copy, and this wharfy writes a different one)",
				Do:     "wharfy init --yes",
			}, true
	}
	return output.Warning{}, output.NextDo{}, false
}

// withInitNudge は管理ブロックが無い/古いなら成功 Result に「次は wharfy init」を一手足す。
// リリースを通した直後(release / publish 一括成功)こそ「次回はエージェントに wharfy で
// やらせたい」と気づく価値が高い。整っていれば何も足さない(冪等・自己沈静)。
func withInitNudge(res output.Result) output.Result {
	warn, next, ok := initAdvice()
	if !ok {
		return res
	}
	res.Warnings = append(res.Warnings, warn)
	res.Next = append(res.Next, next)
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
