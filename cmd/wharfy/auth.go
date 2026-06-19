package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"golang.org/x/term"

	"github.com/ShiroDoromoto/wharfy/internal/output"
	"github.com/ShiroDoromoto/wharfy/internal/registry"
	"github.com/ShiroDoromoto/wharfy/internal/secret"
)

// tokenKind は keychain に預けられる資格情報の種別。env 名と keychain のキー名を結ぶ。
// いまは fury のみ実装。将来 github/aur 等はここに 1 行足すだけで auth と resolveToken が対応する。
type tokenKind struct {
	Kind    string
	EnvVar  string
	KeyName string
	Desc    string
}

var tokenKinds = map[string]tokenKind{
	"fury": {Kind: "fury", EnvVar: "PACKAGE_REPO_TOKEN", KeyName: "package_repo_token", Desc: "fury.io push token (apt/rpm)"},
}

func sortedKinds() []string {
	names := make([]string, 0, len(tokenKinds))
	for k := range tokenKinds {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// resolveToken は env を優先し、無ければ keychain から読む。どちらも無ければ空。
// keychain 読み出しの失敗(NotFound 含む)は空扱い＝publish 側が「未設定」として案内する
// (env で渡す CI 互換を保ちつつ、ローカルは wharfy auth で keychain に預けられる)。
func resolveToken(envVar, keyName string) string {
	if v := os.Getenv(envVar); v != "" {
		return v
	}
	if v, err := secret.Get(keyName); err == nil {
		return v
	}
	return ""
}

// promptSecret はトークンを読む。TTY では隠し入力(画面に出ない・agent context を通らない)、
// 非 TTY ではパイプ等から 1 行。テストで差し替えられるよう var にする。
var promptSecret = func(prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		fmt.Fprint(os.Stderr, prompt)
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// runAuth は資格情報(いまは fury トークン)を OS keychain に保存する。
// 値は hidden prompt から読み、Result には種別だけ載せて値は決して出さない。
func runAuth(_ context.Context, c registry.Command, args []string) output.Result {
	if len(args) == 0 {
		res := output.New(c.Name, "save a credential to the OS keychain — specify a kind: "+strings.Join(sortedKinds(), ", "), true)
		res.Data = map[string]any{"kinds": sortedKinds()}
		res.Next = []output.NextDo{{Reason: "save the fury token (apt/rpm)", Do: "wharfy auth fury"}}
		return res
	}
	kind, ok := tokenKinds[args[0]]
	if !ok {
		res := output.New(c.Name, "unknown credential kind: "+args[0], false)
		res.Errors = []output.Problem{{Code: output.ErrConfigInvalid, Message: "unknown kind " + args[0], Hint: "available: " + strings.Join(sortedKinds(), ", ")}}
		res.Next = []output.NextDo{{Reason: "use a known kind", Do: "wharfy auth fury"}}
		return res
	}

	value, err := promptSecret(kind.Desc + " — paste token (hidden): ")
	if err != nil {
		res := output.New(c.Name, "could not read the token", false)
		res.Errors = []output.Problem{{Code: output.ErrKeychainFailed, Message: "reading token: " + err.Error(), Hint: "run in a terminal, or pipe the token on stdin"}}
		res.Next = []output.NextDo{{Reason: "retry in a terminal", Do: "wharfy auth " + kind.Kind}}
		return res
	}
	if value == "" {
		res := output.New(c.Name, "no token entered", false)
		res.Errors = []output.Problem{{Code: output.ErrTokenMissing, Message: "empty token for " + kind.Kind, Hint: "paste the token at the prompt"}}
		res.Next = []output.NextDo{{Reason: "retry and paste the token", Do: "wharfy auth " + kind.Kind}}
		return res
	}
	if err := secret.Set(kind.KeyName, value); err != nil {
		res := output.New(c.Name, "could not save to the OS keychain", false)
		res.Errors = []output.Problem{{Code: output.ErrKeychainFailed, Message: err.Error(), Hint: "unlock/allow your OS keychain and retry"}}
		res.Next = []output.NextDo{{Reason: "retry after unlocking the keychain", Do: "wharfy auth " + kind.Kind}}
		return res
	}

	res := output.New(c.Name, "saved "+kind.Kind+" token to the OS keychain ("+kind.EnvVar+")", true)
	res.Data = map[string]any{"kind": kind.Kind, "env_var": kind.EnvVar, "stored": true, "storage": "keychain"}
	res.Next = []output.NextDo{{Reason: "publish the package channels (token now resolves from keychain)", Do: "wharfy publish apt --yes"}}
	return res
}
