package main

import (
	"context"
	"os"
	"sort"
	"strings"

	"github.com/ShiroDoromoto/wharfy/internal/config"
	"github.com/ShiroDoromoto/wharfy/internal/output"
	"github.com/ShiroDoromoto/wharfy/internal/registry"
	"github.com/ShiroDoromoto/wharfy/internal/sign"
)

// runSign は署名/公証の状態を可視化し案内する(設計 10 / ADR-7)。実行はしない。
// 未署名はブロックしない(署名は要件でなく品質)。正直に「advisory」と伝える(no-op 偽装をしない)。
func runSign(_ context.Context, c registry.Command, _ []string) output.Result {
	root, err := os.Getwd()
	if err != nil {
		return internalError(c, err)
	}
	in, _ := config.Load(root)
	cfg, _ := config.NewResolver(root).Resolve(in) // main 曖昧でも sign は出せる(ビルドしない)

	goos := config.DefaultGOOS
	if cfg.Build != nil && len(cfg.Build.GOOS) > 0 {
		goos = cfg.Build.GOOS
	}
	status := sign.Status(goos)

	res := output.New(c.Name, signMessage(status), true)
	res.Data = map[string]any{"sign": status}

	// 正準コードがある windows 未署名のみ warning にする(darwin 未署名は sign ブロックで機械可読)。
	if t, ok := status["windows"]; ok && !t.Signed {
		res.Warnings = append(res.Warnings, output.Warning{
			Code:    output.WarnWinUnsigned,
			Message: "windows unsigned — no certificate configured",
		})
	}

	// MVP は署名を実行しないので「export して sign すれば署名される」とは案内しない(no-op 偽装の回避)。
	// 未署名は警告のまま publish 可能。
	res.Next = []output.NextDo{
		{Reason: "unsigned is a warning, not a blocker — continue to publish", Do: "wharfy publish homebrew"},
	}
	return res
}

// signMessage は advisory であることを明示しつつ各 OS の状態を一行に畳む。
func signMessage(status map[string]sign.Target) string {
	oses := make([]string, 0, len(status))
	for os := range status {
		oses = append(oses, os)
	}
	sort.Strings(oses)
	parts := make([]string, 0, len(oses))
	for _, os := range oses {
		t := status[os]
		switch {
		case t.Signed && t.Notarized:
			parts = append(parts, os+" signed+notarized")
		case t.Signed:
			parts = append(parts, os+" signed")
		default:
			parts = append(parts, os+" unsigned")
		}
	}
	if len(parts) == 0 {
		return "signing: nothing to sign (advisory only; wharfy does not sign in MVP)"
	}
	return "signing (advisory only; wharfy does not sign in MVP): " + strings.Join(parts, ", ")
}
