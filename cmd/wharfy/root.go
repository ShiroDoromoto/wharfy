package main

import (
	"errors"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ShiroDoromoto/wharfy/internal/output"
	"github.com/ShiroDoromoto/wharfy/internal/registry"
)

// errNotOK は Result.OK=false のとき RunE から返す番兵。プロセスを非ゼロ終了させるためだけの
// もので、追加メッセージは出さない(envelope は Emit 済み)。main が握って exit(1) する。
// これで --json の ok:false が終了コードにも一致する(利用者が指摘した「ok:false なのに exit 0」直し)。
var errNotOK = errors.New("command reported ok=false")

// 共通グローバルフラグ(CLI 層)。全コマンドが受ける。
var (
	flagJSON       bool
	flagDryRun     bool
	flagYes        bool
	flagNoProbe    bool
	flagInstall    bool
	flagAckReview  bool
	flagAllowStale bool
)

// newRootCmd は registry から cobra コマンドツリーを生成する。
// コマンド本体は薄く、registry を単一真実に保つ(drift 対策)。
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
	root.PersistentFlags().BoolVar(&flagInstall, "install", false, "verify: install from each channel and run it (default: probe only)")
	root.PersistentFlags().BoolVar(&flagAckReview, "acknowledge-review", false, "strict gated channels (e.g. homebrew-core): acknowledge you meet the acceptance criteria before opening a PR")
	root.PersistentFlags().BoolVar(&flagAllowStale, "allow-stale-generator", false, "release/publish --yes: ship artifacts made by the running wharfy even though it is not built from this repo's HEAD")

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
			if !res.OK {
				return errNotOK
			}
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
