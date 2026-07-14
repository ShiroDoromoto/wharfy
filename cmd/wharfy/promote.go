package main

// promote.go — 検証の終わった prerelease を latest に昇格する工程(release --prerelease の対)。
//
// 昇格は **latest のフラグを立てるだけ**で、資産も来歴も作り直さない。だから「検証したバイト列が
// そのまま配られる」と言い切れる —— 昇格のたびに何かを作り直していたら、検証した物と配る物が
// 別になりうる。来歴(attest)を release の時点で付けているのはこのためでもある。

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/ShiroDoromoto/wharfy/internal/channel"
	"github.com/ShiroDoromoto/wharfy/internal/config"
	"github.com/ShiroDoromoto/wharfy/internal/output"
	"github.com/ShiroDoromoto/wharfy/internal/registry"
	"github.com/ShiroDoromoto/wharfy/internal/state"
)

// promoteData は promote の固有ペイロード。
type promoteData struct {
	Applied bool   `json:"applied"`
	Target  string `json:"target,omitempty"`
	Version string `json:"version,omitempty"`
	// Promoted は**この実行が**切り替えたか。既に latest だった(冪等な二度目)なら false。
	Promoted bool `json:"promoted"`
	// Latest は昇格後の状態(この版が latest か)。冪等な二度目でも true になる。
	Latest bool `json:"latest"`
}

// runPromote は prerelease を GitHub の latest にする。冪等 —— 既に latest なら何もせず緑。
func runPromote(ctx context.Context, c registry.Command, _ []string) output.Result {
	root, err := os.Getwd()
	if err != nil {
		return internalError(c, err)
	}
	in, loadErr := config.Load(root)
	if loadErr != nil {
		return configInvalidResult(c, loadErr)
	}
	cfg, rerr := config.NewResolver(root).Resolve(in)
	var ambiguous *config.AmbiguousMainError
	if errors.As(rerr, &ambiguous) {
		return mainAmbiguousResult(c, cfg, ambiguous)
	}
	if rerr != nil {
		return internalError(c, rerr)
	}
	version, tagMissing := publishVersion(root)

	if !flagYes {
		target := cfg.Github
		if target == "" {
			target = "(github unresolved)"
		}
		res := output.New(c.Name, "plan: make v"+version+" the latest release → "+target, true)
		res.Data = promoteData{Applied: false, Target: cfg.Github, Version: version}
		var next []output.NextDo
		if tagMissing {
			next = append(next, output.NextDo{Reason: "required before --yes: git tag", Do: "git tag vX.Y.Z && git push --tags (the tag is the version)"})
		}
		if os.Getenv("GITHUB_TOKEN") == "" {
			next = append(next, output.NextDo{Reason: "required before --yes: GITHUB_TOKEN", Do: "export GITHUB_TOKEN=… (promote edits the release)"})
		}
		next = append(next, output.NextDo{
			Reason: "check what you are about to hand to users, from the consumer side",
			Do:     "wharfy verify --version " + version,
		})
		res.Next = append(next, output.NextDo{Reason: "make it latest", Do: "wharfy promote --yes"})
		return res
	}

	if tagMissing {
		return tagMissingResult(c, version)
	}
	if os.Getenv("GITHUB_TOKEN") == "" {
		return tokenMissingResult(c)
	}
	owner, repo, ok := splitOwnerName(cfg.Github)
	if !ok {
		res := output.New(c.Name, "cannot promote: github owner/repo is unresolved", false)
		res.Errors = []output.Problem{{
			Code:    output.ErrGithubUnresolved,
			Message: "cannot work out the owner/repo to promote on (github: " + cfg.Github + ")",
			Hint:    "set github: owner/repo in wharfy.yaml, or add an origin remote",
		}}
		return res
	}
	changed, perr := newReleaseStore(owner, repo, os.Getenv("GITHUB_TOKEN")).Promote(ctx, "v"+version)
	if errors.Is(perr, channel.ErrNoRelease) {
		return noReleaseToPromoteResult(c, cfg, version)
	}
	if perr != nil {
		return promoteFailedResult(c, cfg, version, perr)
	}
	// 台帳の「上げただけ」の印を落とす —— ここで初めて利用者に届く。
	clearPrereleaseRecords(root, cfg, version)

	msg := "promoted " + cfg.Project + " " + version + ": it is now the latest release → " + cfg.Github
	if !changed {
		msg = cfg.Project + " " + version + " is already the latest release → " + cfg.Github + " (nothing to do)"
	}
	res := output.New(c.Name, msg, true)
	res.Data = promoteData{Applied: true, Target: cfg.Github, Version: version, Promoted: changed, Latest: true}
	res.Next = nextFromSpec(c) // publish
	return res
}

// clearPrereleaseRecords は releases / script の記録から prerelease の印を落とす(昇格＝利用者に届いた)。
// 記録が無ければ何もしない —— 昇格は release と別のジョブ(別のマシン)で走りうるので、台帳が
// 手元に無いことは異常ではない。判断は常に GitHub の実体が正で、台帳はその写しに過ぎない。
func clearPrereleaseRecords(root string, cfg config.Config, version string) {
	st, err := state.Load(root, cfg.Project)
	if err != nil || st.Publish == nil {
		return
	}
	now := nowUTC().Format(time.RFC3339)
	for _, ch := range []string{"releases", "script"} {
		rec, ok := st.Publish[ch]
		if !ok || rec.Version != version || !rec.Prerelease {
			continue
		}
		rec.Prerelease = false
		rec.At = now
		st.Publish[ch] = rec
	}
	_ = state.Save(root, st)
}

// noReleaseToPromoteResult は「昇格しようとしたが、その版のリリースがまだ無い」拒否。
func noReleaseToPromoteResult(c registry.Command, cfg config.Config, version string) output.Result {
	res := output.New(c.Name, "nothing to promote: no release for v"+version, false)
	res.Errors = []output.Problem{{
		Code:    output.ErrNoRelease,
		Message: "there is no github release for v" + version + " on " + cfg.Github + ": promotion flips a flag on a release that already exists",
		Hint:    "upload it first (wharfy release --yes --prerelease), verify it, then promote",
	}}
	res.Next = []output.NextDo{{Reason: "upload the artifacts you want to hand to users", Do: "wharfy release --yes --prerelease"}}
	return res
}

// promoteFailedResult は昇格そのものの失敗(権限/ネットワーク)。資産は上がったままなので、
// 直して打ち直せばよい —— 昇格はフラグを立てるだけで、何も作り直さない(冪等)。
func promoteFailedResult(c registry.Command, cfg config.Config, version string, err error) output.Result {
	res := output.New(c.Name, "failed to promote v"+version, false)
	res.Errors = []output.Problem{{
		Code:    output.ErrPublishFailed,
		Message: "could not make v" + version + " the latest release on " + cfg.Github,
		Detail:  err.Error(),
		Hint:    "the assets are untouched: fix the cause (token scope: contents: write) and run it again — promote is idempotent",
	}}
	res.Next = []output.NextDo{{Reason: "retry the promotion", Do: "wharfy promote --yes"}}
	return res
}
