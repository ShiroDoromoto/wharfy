package main

import (
	"context"
	"os"

	"github.com/ShiroDoromoto/wharfy/internal/channel"
	"github.com/ShiroDoromoto/wharfy/internal/config"
	"github.com/ShiroDoromoto/wharfy/internal/output"
	"github.com/ShiroDoromoto/wharfy/internal/registry"
	"github.com/ShiroDoromoto/wharfy/internal/state"
)

// runVerify は発行済み owned チャネルの到達性・整合性を確認する(設計 01 verify / 06)。
// スライス1 は homebrew のみ: 自前 tap の formula が在り、版が記録と一致するかを照合する。
// 未発行なら「確認対象なし」を正直に返し、publish を促す(空 next の dead-end を作らない)。
func runVerify(ctx context.Context, c registry.Command, _ []string) output.Result {
	root, err := os.Getwd()
	if err != nil {
		return internalError(c, err)
	}
	in, _ := config.Load(root)
	cfg, _ := config.NewResolver(root).Resolve(in)
	st, _ := state.Load(root, cfg.Project)

	rec, has := st.Publish["homebrew"]
	if !has || rec.Version == "" {
		res := output.New(c.Name, "nothing published to verify yet", true)
		res.Next = []output.NextDo{{Reason: "publish first, then verify the install", Do: "wharfy publish homebrew --yes"}}
		return res
	}

	tap := firstNonEmptyStr(rec.Target, homebrewTargetOrEmpty(cfg))
	owner, repo, ok := splitOwnerName(tap)
	if !ok {
		return internalError(c, errString("recorded homebrew target is unresolved: "+tap))
	}

	hb := &channel.Homebrew{
		Project: cfg.Project,
		Tap:     tap,
		Store:   newTapStore(owner, repo, os.Getenv("GITHUB_TOKEN")),
	}
	rs, perr := hb.Probe(ctx)
	if perr != nil {
		res := output.New(c.Name, "cannot reach the tap to verify", false)
		res.Errors = []output.Problem{{Code: output.ErrProbeFailed, Message: perr.Error(), Hint: "check network or tap visibility"}}
		res.Next = []output.NextDo{{Reason: "retry once reachable", Do: "wharfy verify"}}
		return res
	}

	switch {
	case !rs.Found:
		res := output.New(c.Name, "verify failed: homebrew recorded "+rec.Version+" but no formula at "+tap, false)
		res.Errors = []output.Problem{{
			Code:    output.ErrVerifyFailed,
			Message: "published formula not found on the tap",
			Hint:    "re-publish to restore the formula",
		}}
		res.Next = []output.NextDo{{Reason: "re-publish the missing formula", Do: "wharfy publish homebrew --yes"}}
		return res
	case rs.Version != rec.Version:
		res := output.New(c.Name, "verify failed: tap has "+rs.Version+", expected "+rec.Version, false)
		res.Errors = []output.Problem{{
			Code:    output.ErrVerifyFailed,
			Message: "tap formula version does not match the published record",
			Hint:    "re-publish to align the tap with the recorded version",
		}}
		res.Next = []output.NextDo{{Reason: "align the tap with the record", Do: "wharfy publish homebrew --yes"}}
		return res
	default:
		res := output.New(c.Name, "homebrew "+rs.Version+" verified: formula present at "+tap+", version matches record", true)
		res.Next = []output.NextDo{{Reason: "distribution looks consistent; review overall state", Do: "wharfy status"}}
		return res
	}
}

// homebrewTargetOrEmpty は cfg の homebrew tap(無ければ空)。
func homebrewTargetOrEmpty(cfg config.Config) string {
	t, _ := homebrewTarget(cfg)
	return t
}

type errString string

func (e errString) Error() string { return string(e) }
