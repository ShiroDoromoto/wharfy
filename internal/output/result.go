// Package output は全コマンド共通の戻り(Result envelope)と、
// 人間向け / --json を 1 か所で出し分ける emit を持つ(設計 01 出力契約層 / 02)。
//
// ここが「出力は契約」の実体。フィールドの形は schemas/result.json に厳密に従う。
// 破壊的変更は schema_version で管理する(削除・意味変更で繰り上げ)。
package output

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// SchemaVersion は v1 バンドル内の全出力で固定(schemas/common.json の const)。
const SchemaVersion = "1"

// Result は全コマンド共通の戻り。--json はこれをそのまま出す(schemas/result.json)。
//
// 並び順は出力契約 02 の envelope に合わせる。omitempty を付けないフィールド
// (schema_version / command / ok / message / next)は schema で required。
type Result struct {
	SchemaVersion string    `json:"schema_version"`
	Command       string    `json:"command"`
	OK            bool      `json:"ok"`
	Message       string    `json:"message"`
	Data          any       `json:"data,omitempty"`
	Warnings      []Warning `json:"warnings,omitempty"`
	Errors        []Problem `json:"errors,omitempty"`
	Next          []NextDo  `json:"next"`
}

// NextDo は次の一手。Do はそのまま実行できるコマンド文字列(環境変数設定を含みうる)。
type NextDo struct {
	Reason string `json:"reason"`
	Do     string `json:"do"`
}

// Warning は非致命の注意。AI が自由文でなくコードで分岐できるようコード付き(09)。
type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Problem は致命エラー(ok=false の時)。Hint は任意の解消ヒント(09)。
type Problem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

// New は schema_version と最低限の next を埋めた Result を作る。
// Result を組み立てる箇所はここを通し、schema_version の付け忘れを防ぐ。
func New(command, message string, ok bool) Result {
	return Result{
		SchemaVersion: SchemaVersion,
		Command:       command,
		OK:            ok,
		Message:       message,
		Next:          []NextDo{},
	}
}

// Emit は人間向けと --json を 1 か所で出し分ける(samples/cmd_agent.go の emit 準拠)。
// next: の体裁もここで統一する。
func Emit(r Result, asJSON bool) {
	emitTo(os.Stdout, r, asJSON)
}

func emitTo(w io.Writer, r Result, asJSON bool) {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(r)
		return
	}
	mark := "✓"
	if !r.OK {
		mark = "✗"
	}
	fmt.Fprintf(w, "%s %s\n", mark, r.Message)
	for _, e := range r.Errors {
		fmt.Fprintf(w, "  ! %s: %s\n", e.Code, e.Message)
		if e.Hint != "" {
			fmt.Fprintf(w, "    %s\n", e.Hint)
		}
	}
	for _, wn := range r.Warnings {
		fmt.Fprintf(w, "  ⚠ %s: %s\n", wn.Code, wn.Message)
	}
	if len(r.Next) > 0 {
		fmt.Fprintln(w, "next:")
		for _, n := range r.Next {
			fmt.Fprintf(w, "  %-28s # %s\n", n.Do, n.Reason)
		}
	}
}

// Marshal は --json の正準表現(SetIndent("", "  "))を文字列で返す。
// golden snapshot やテストで Emit と同じ整形を使うため。
func Marshal(v any) (string, error) {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return b.String(), nil
}
