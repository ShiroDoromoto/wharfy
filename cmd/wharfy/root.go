package main

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/ShiroDoromoto/wharfy/internal/output"
	"github.com/ShiroDoromoto/wharfy/internal/registry"
)

// 共通グローバルフラグ(設計 01 CLI 層)。全コマンドが受ける。
var (
	flagJSON      bool
	flagDryRun    bool
	flagYes       bool
	flagNoProbe   bool
	flagAckReview bool
)

// newRootCmd は registry から cobra コマンドツリーを生成する。
// コマンド本体は薄く、registry を単一真実に保つ(05 drift 対策)。
// テストからも同じツリーを組み立てられるよう関数に分ける。
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "wharfy",
		Short:         "ship one binary to every channel. Read `wharfy agent` once, then drive.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().BoolVar(&flagJSON, "json", false, "machine-readable output (see schemas/)")
	root.PersistentFlags().BoolVar(&flagDryRun, "dry-run", false, "show what would change; write nothing")
	root.PersistentFlags().BoolVar(&flagYes, "yes", false, "apply changes to owned distribution (publish writes the tap)")
	root.PersistentFlags().BoolVar(&flagNoProbe, "no-probe", false, "status: read records only; do not probe channel reality")
	root.PersistentFlags().BoolVar(&flagAckReview, "acknowledge-review", false, "strict gated channels (e.g. homebrew-core): acknowledge you meet the acceptance criteria before opening a PR")

	for _, c := range registry.Commands {
		root.AddCommand(newCommand(c))
	}
	return root
}

// newCommand は registry の 1 エントリから cobra.Command を作る。
func newCommand(c registry.Command) *cobra.Command {
	use := c.Name
	if c.Args != "" {
		use += " " + c.Args
	}
	return &cobra.Command{
		Use:   use,
		Short: c.Summary,
		RunE: func(cmd *cobra.Command, args []string) error {
			// agent / status は Result envelope と別形(agent.json / status.json)なので特別扱い。
			if c.Name == "agent" {
				return runAgent(flagJSON)
			}
			if c.Name == "status" {
				return runStatus(cmd.Context(), flagJSON)
			}
			res := dispatch(cmd.Context(), c, args)
			output.Emit(res, flagJSON)
			return nil
		},
	}
}

// nextFromSpec は registry の既定 next 名を、そのまま実行できる NextDo に展開する。
// スライス1 のスタブ段階で next: 体裁を成立させるための最小実装。
func nextFromSpec(c registry.Command) []output.NextDo {
	next := make([]output.NextDo, 0, len(c.Next))
	for _, n := range c.Next {
		spec, _ := registry.Lookup(n)
		next = append(next, output.NextDo{
			Reason: strings.ToLower(spec.Summary),
			Do:     "wharfy " + n,
		})
	}
	return next
}
