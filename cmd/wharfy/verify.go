package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/ShiroDoromoto/wharfy/internal/channel"
	"github.com/ShiroDoromoto/wharfy/internal/config"
	"github.com/ShiroDoromoto/wharfy/internal/output"
	"github.com/ShiroDoromoto/wharfy/internal/registry"
	"github.com/ShiroDoromoto/wharfy/internal/state"
)

// verify.go — wharfy.yaml の channels: にあるチャネルを消費側から確かめる。
//
// 検証の対象集合は `channels:` が決める(D-4)。state.json の publish 履歴は監査記録であって
// 行動の根拠にしない — 設定から外したチャネルの古い記録が残っていても検証しない。畳んだ tap を
// 検証して緑を返し、現行チャネルを一つも見ていない、という嘘をつかないため。
//
// 既定は probe だけ ——ネットワーク越しの照合で終わり、何もインストールしない(D-4)。CI で毎回
// 叩けるように軽く保つ。実インストールまで踏むのは `--install` を明示したときに限る(人間の決定)。
//
// homebrew は自前 tap の formula の、scoop は自前 bucket の manifest の、有無と版を照合する。
// releases は Release の資産マニフェスト(latest.json / checksums)が載せる資産が実在するかを照合する
// (本体は落とさない・D-4)。この 3 つは verify が踏める最大をここで踏み切っている(scoop の install は
// Windows でしか踏めないが、bucket の manifest は HTTP で読めるので Linux の CI でも見張れる)。
// 残る apt / rpm / script / goinstall には「実際に入れて動かす」余地があり、既定ではそこを踏まないので
// partial に落とす。
//
// `--install` はその余地を踏む。apt/rpm は使い捨てコンテナで repo を足して install する。script は
// 一時 PREFIX へ install.sh を、goinstall は一時 GOBIN へ go install を走らせる(→ verify_install.go)。
// 供給側(hosted repo への push、Release へのアップロード)はアップロードが 200 を返せば成功するので、
// 生成物の依存やファイル配置が壊れていても気づけない。踏むのは利用者になる。
//
// `wharfy verify [channel]` は対象を 1 チャネルに絞る。channels: に無い名前は publish と同じ
// channel_not_configured で拒む。
//
// 一つも検証できなかったときは ok=false(nothing_to_verify)。「確かめられなかった」を緑で返すと、
// CI がそれを「配布は健全」と読んでしまう(D-4)。

// verifyCheck は 1 チャネル分の検証結果(verify の data)。
type verifyCheck struct {
	Channel string `json:"channel"`
	Status  string `json:"status"` // verified / partial / failed / skipped
	Message string `json:"message"`
}

// verifyData は verify の data ペイロード。version は「何を確かめたか」、version_source は
// その版をどこから決めたか(利用者が結果を読むとき、基点が記録か実体かで意味が変わる)。
type verifyData struct {
	Version       string        `json:"version,omitempty"`
	VersionSource string        `json:"version_source,omitempty"`
	Checks        []verifyCheck `json:"checks"`
}

// 版の出どころ(verifyData.VersionSource)。
const (
	verifySourceRequested = "requested" // --version で明示された
	verifySourceRecord    = "record"    // .wharfy/state.json の publish 記録
	verifySourceRelease   = "release"   // GitHub Release の最新版(＝実際に配ってあるもの)
	verifySourceTag       = "tag"       // git の直近タグ
)

// verifyTarget は 1 チャネルを確かめるときの「期待」——どの版を、どこで、その版はどこから来たか。
//
// 以前は publish 記録(.wharfy/state.json)だけが基点だった。記録は生成物なので gitignore され、
// CI の別ジョブにもまっさらな clone にも渡らない —— 結果、配った後に「今も入るか」を確かめようと
// すると、ほぼ全チャネルが skipped になって何も確かめられなかった。基点は記録に限らない。
type verifyTarget struct {
	Version string
	Target  string // 記録に残る書き先(設定で解決できないときのフォールバック)
	Source  string
	// Behind は実体を採ったときに、それより古かった publish 記録の版(無ければ空)。
	// CI で publish すると記録は runner 側にしか残らず、手元の記録は古いまま —— 配布は正常なのに
	// verify だけが赤くなる。実体に倒したうえで、記録が陳腐化していることを言うために持つ。
	Behind string
}

// expected は失敗メッセージで使う「期待の版と、その根拠」。
func (t verifyTarget) expected() string {
	switch t.Source {
	case verifySourceRequested:
		return t.Version + " (requested)"
	case verifySourceRelease:
		return t.Version + " (the latest github release)"
	case verifySourceTag:
		return t.Version + " (the latest git tag)"
	default:
		return t.Version + " (the published record)"
	}
}

const (
	verifyStatusOK = "verified"
	// verifyStatusPartial は「検証は走ったが、最後まで踏めなかった」。--install を付けなかった
	// (既定・probe まで)か、付けたが道具が無かった(docker / sh / go 不在)かのどちらか。失敗ではない
	// ので ok は落とさないが、verified とも呼ばない。
	// 検証対象ゼロ(nothing_to_verify)の判定では「走った」側に数える。
	verifyStatusPartial = "partial"
	verifyStatusFailed  = "failed"
	verifyStatusSkipped = "skipped"
)

// verifyOutcome は 1 チャネルの検証結果と、それが呼ぶ envelope 要素(問題・警告・次の一手)。
type verifyOutcome struct {
	check   verifyCheck
	problem *output.Problem
	warning *output.Warning
	next    *output.NextDo
}

var (
	// dockerRun はコンテナ実行の末端(テストで差し替え)。CombinedOutput を返す。
	dockerRun = func(ctx context.Context, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	}
	// defaultVerifyImages は apt/rpm を確かめるベースイメージの既定。利用者の環境の代表として
	// debian/fedora を踏む。実際に配る先が違うなら wharfy.yaml の verify.images で名指しする。
	defaultVerifyImages = map[string]string{"apt": "debian:12", "rpm": "fedora:40"}
	// verifyTimeout は実インストール 1 本の上限。apt-get update / dnf のメタデータ取得も、
	// go install の初回ビルドも遅い。
	verifyTimeout = 10 * time.Minute
)

// runVerify は channels: にあるチャネルの到達性・整合性を確認する(verify)。
// 引数でチャネルを1つ名指しできる(省略時は channels: の全部)。--install はコンテナやインストーラを
// 起こすので、1 チャネルを直している間は他を走らせない方が反復が軽い。
// 未発行・未対応のチャネルは skip として checks に載せ、一つも検証できなければ ok=false を返す。
func runVerify(ctx context.Context, c registry.Command, args []string) output.Result {
	root, err := os.Getwd()
	if err != nil {
		return internalError(c, err)
	}
	in, loadErr := config.Load(root)
	if loadErr != nil {
		return configInvalidResult(c, loadErr)
	}
	cfg, _ := config.NewResolver(root).Resolve(in)
	st, _ := state.Load(root, cfg.Project)

	targets := cfg.Channels
	if len(args) > 0 {
		sel, ok := selectChannel(cfg, args[0])
		if !ok {
			return verifyChannelNotConfigured(c, cfg, in, args[0])
		}
		targets = []config.ResolvedChannel{sel}
	}

	// 記録に頼らない基点(実体 → git のタグ)。チャネルごとに問い合わせず、1 度だけ決めて使い回す。
	var (
		fallback verifyTarget
		resolved bool
	)
	fallbackFor := func() verifyTarget {
		if !resolved {
			fallback, resolved = resolveFallbackVersion(ctx, root, cfg, st), true
		}
		return fallback
	}

	var outcomes []verifyOutcome
	var unpublished []string
	var used []verifyTarget
	for _, ch := range targets {
		tgt := verifyTargetFor(st, ch, fallbackFor)
		if tgt.Version == "" {
			unpublished = append(unpublished, ch.Name)
			outcomes = append(outcomes, verifyNotRun(ch.Name, ch.Name+" skipped: nothing published on this channel yet"))
			continue
		}
		used = append(used, tgt)
		switch ch.Name {
		case "goinstall":
			outcomes = append(outcomes, verifyGoinstall(ctx, cfg, in, tgt))
		case "homebrew":
			oc, err := verifyHomebrew(ctx, cfg, ch, tgt)
			if err != nil {
				return internalError(c, err)
			}
			outcomes = append(outcomes, oc)
		case "scoop":
			oc, err := verifyScoop(ctx, cfg, in, ch, tgt)
			if err != nil {
				return internalError(c, err)
			}
			outcomes = append(outcomes, oc)
		case "releases":
			oc, err := verifyReleases(ctx, ch, tgt)
			if err != nil {
				return internalError(c, err)
			}
			outcomes = append(outcomes, oc)
		case "script":
			outcomes = append(outcomes, verifyScript(ctx, cfg, in, tgt))
		case "apt", "rpm":
			outcomes = append(outcomes, verifyLinuxRepo(ctx, ch, cfg, in, tgt))
		default:
			outcomes = append(outcomes, verifyNotRun(ch.Name, ch.Name+" skipped: verify does not cover this channel yet"))
		}
	}
	return verifyResult(c, outcomes, unpublished, used)
}

// verifyTargetFor は 1 チャネルの期待(版・書き先・その出どころ)を決める。
//
//	--version が在ればそれ(利用者が「確かめたい版」を名指しした)。
//	無ければそのチャネルの publish 記録 —— ただし記録より新しい実体が在るなら、実体を採る。
//	記録も無ければ実体から決めた fallback —— ここが「まっさらな clone でも確かめられる」の要。
//
// 記録より実体を優先するのは、記録(.wharfy/state.json)が生成物だから。CI で publish すると記録は
// runner 側にしか残らず、手元は古い版のまま —— それを信じると、配布は正常なのに verify だけが
// 赤くなる。実体(GitHub Release の最新版)は誰から見ても同じで、配ってあるものそのもの。
// ただし倒す先は実体に限る: タグは打っただけで配っていない版でもありうるので、記録の方が確かな根拠。
func verifyTargetFor(st *state.State, ch config.ResolvedChannel, fallback func() verifyTarget) verifyTarget {
	rec, published := publishedRecord(st, ch.Name)
	if flagVerifyVersion != "" {
		return verifyTarget{Version: strings.TrimPrefix(flagVerifyVersion, "v"), Target: rec.Target, Source: verifySourceRequested}
	}
	// goinstall は何も push しないチャネルなので publish 記録を持たない(基点は必ず fallback)。
	if !published || ch.Name == "goinstall" {
		return fallback()
	}
	if fb := fallback(); fb.Source == verifySourceRelease && state.CompareVersions(fb.Version, rec.Version) > 0 {
		return verifyTarget{Version: fb.Version, Target: rec.Target, Source: verifySourceRelease, Behind: rec.Version}
	}
	return verifyTarget{Version: rec.Version, Target: rec.Target, Source: verifySourceRecord}
}

// resolveFallbackVersion は記録に頼らず「何を確かめるか」を実体から決める。
//
// 一番強い根拠は GitHub Release の最新版 —— 実際に配ってあるものそのもので、ローカルに何も
// 要らない。取れなければ git の直近タグ(HEAD がタグより進んでいても、配ったのは直近のタグ)、
// それも無ければ wharfy が最後に見たタグ。全部空なら未発行として skip する。
func resolveFallbackVersion(ctx context.Context, root string, cfg config.Config, st *state.State) verifyTarget {
	if owner, repo, ok := splitOwnerName(cfg.Github); ok {
		if v, found, err := newReleasesProbe(owner, repo).Latest(ctx); err == nil && found {
			return verifyTarget{Version: v, Source: verifySourceRelease}
		}
	}
	if tag := firstNonEmptyStr(gitCurrentTag(root), gitLatestTag(root), st.LastTag); tag != "" {
		return verifyTarget{Version: strings.TrimPrefix(tag, "v"), Source: verifySourceTag}
	}
	return verifyTarget{}
}

// gitLatestTag は直近のタグを返す(HEAD がタグより進んでいても「配ったもの」に当たれる)。
// 浅い clone にはタグが来ないので空になる —— そのときは Release 側の実体が基点になる。
var gitLatestTag = func(root string) string {
	out, err := exec.Command("git", "-C", root, "describe", "--tags", "--abbrev=0").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// selectChannel は名指しされたチャネルを channels: から引く。
func selectChannel(cfg config.Config, name string) (config.ResolvedChannel, bool) {
	for _, ch := range cfg.Channels {
		if ch.Name == name {
			return ch, true
		}
	}
	return config.ResolvedChannel{}, false
}

// verifyChannelNotConfigured は名指しされたチャネルが channels: に無いときの拒否。publish と同じ
// 語彙で断る(D-4: 宣言した集合だけが対象)。畳んだチャネルを検証して緑を返さないため。
//
// publish は「未実装チャネル」へのディスパッチを持つので綴り違いをそこで拾うが、verify の対象は
// 常に channels: 由来なので、綴り違いも「設定に無い」に落ちる。直し方だけ hint で分ける
// ——書き戻せと言われても、そんな名前のチャネルは存在しない。
func verifyChannelNotConfigured(c registry.Command, cfg config.Config, in config.File, name string) output.Result {
	res := output.New(c.Name, name+" is not in wharfy.yaml channels: — nothing verified", false)
	res.Data = verifyData{Checks: []verifyCheck{}}
	prob := channelNotConfiguredProblem(cfg, in, name)
	if !config.KnownChannel(name) {
		prob.Hint = "there is no wharfy channel named '" + name + "' — check the spelling"
	}
	res.Errors = []output.Problem{prob}
	res.Next = []output.NextDo{{Reason: "verify the channels you declared", Do: "wharfy verify"}}
	return res
}

// publishedRecord は state から発行済みの記録を返す(版が空なら未発行扱い)。
func publishedRecord(st *state.State, name string) (state.PublishRecord, bool) {
	rec, has := st.Publish[name]
	if !has || rec.Version == "" {
		return state.PublishRecord{}, false
	}
	return rec, true
}

// verifyResult は各チャネルの結果を 1 つの envelope に畳む。
//
//	failed が 1 つでもあれば ok=false。
//	検証が 1 つも走らなければ ok=false(nothing_to_verify) — skip だけで緑を返さない。
//	partial は「走ったが最後まで踏めなかった」なので、ok を落とさず対象ゼロにも数えない。
//
// unpublished は channels: にあるが未発行のチャネル(検証対象ゼロのときの次の一手に使う)。
// used は各チャネルが確かめた期待(版と出どころ)。全部が同じ版なら data に載せる。
func verifyResult(c registry.Command, outcomes []verifyOutcome, unpublished []string, used []verifyTarget) output.Result {
	var checks []verifyCheck
	var problems []output.Problem
	var warnings []output.Warning
	var nexts []output.NextDo
	failed, exercised := false, 0
	for _, oc := range outcomes {
		checks = append(checks, oc.check)
		switch oc.check.Status {
		case verifyStatusFailed:
			failed = true
		case verifyStatusOK, verifyStatusPartial:
			exercised++
		}
		if oc.problem != nil {
			problems = append(problems, *oc.problem)
		}
		if oc.warning != nil {
			warnings = append(warnings, *oc.warning)
		}
		if oc.next != nil {
			nexts = append(nexts, *oc.next)
		}
	}

	nothingToVerify := !failed && exercised == 0
	res := output.New(c.Name, verifyMessage(checks, nothingToVerify), !failed && !nothingToVerify)
	data := verifyData{Checks: checks}
	if v, src, uniform := uniformTarget(used); uniform {
		data.Version, data.VersionSource = v, src
	}
	res.Data = data
	if w := staleRecordWarning(used); w != nil {
		warnings = append(warnings, *w)
	}
	res.Warnings = warnings
	res.Errors = problems
	switch {
	case failed:
		res.Next = nexts
	case nothingToVerify:
		res.Errors = append(res.Errors, output.Problem{
			Code:    output.ErrNothingToVerify,
			Message: "no channel in wharfy.yaml could be verified",
			Hint:    "publish a channel that verify covers, or check why every channel was skipped",
		})
		res.Next = nothingToVerifyNext(unpublished)
	default:
		res.Next = verifiedNext(checks)
	}
	return res
}

// staleRecordWarning は「手元の publish 記録が実体より古い」を 1 度だけ言う。
//
// 検証は実体に倒して緑のまま通るが、黙って倒すと記録の陳腐化に配布者が気づけない —— status の
// 記録と実体の食い違い(drift)と同じ事象なので、コードも同じものを使う。
func staleRecordWarning(used []verifyTarget) *output.Warning {
	for _, t := range used {
		if t.Behind == "" {
			continue
		}
		return &output.Warning{
			Code: output.WarnDriftDetected,
			Message: "the local publish record (" + t.Behind + ") is behind the latest release (" + t.Version +
				") — verified " + t.Version + "; .wharfy/state.json is stale, which is expected when publish runs in CI",
		}
	}
	return nil
}

// uniformTarget は確かめた期待が全チャネルで同じ版かを見る。版がばらけている(記録が
// チャネルごとに違う)なら 1 つの版を名乗れないので載せない —— 嘘をつくより黙る。
func uniformTarget(used []verifyTarget) (version, source string, uniform bool) {
	if len(used) == 0 {
		return "", "", false
	}
	for _, t := range used[1:] {
		if t.Version != used[0].Version || t.Source != used[0].Source {
			return "", "", false
		}
	}
	return used[0].Version, used[0].Source, true
}

// verifiedNext は緑のときの次の一手。probe で止まったチャネルが残っているなら、まず実インストール
// を勧める ——「verify が緑」と「利用者が入れられる」は別の主張で、後者は --install でしか言えない。
func verifiedNext(checks []verifyCheck) []output.NextDo {
	var next []output.NextDo
	if !flagInstall && hasStatus(checks, verifyStatusPartial) {
		next = append(next, output.NextDo{Reason: "the installs were probed but never exercised", Do: "wharfy verify --install"})
	}
	return append(next, output.NextDo{Reason: "distribution looks consistent; review overall state", Do: "wharfy status"})
}

// hasStatus は checks にその状態が 1 つでもあるか。
func hasStatus(checks []verifyCheck, status string) bool {
	for _, ck := range checks {
		if ck.Status == status {
			return true
		}
	}
	return false
}

// nothingToVerifyNext は検証対象ゼロのときの次の一手。channels: にあるチャネルだけを勧める
// (設定から外したチャネルへの publish を勧めない・D-4)。
func nothingToVerifyNext(unpublished []string) []output.NextDo {
	if len(unpublished) > 0 {
		return []output.NextDo{{Reason: "publish first, then verify the install", Do: "wharfy publish " + unpublished[0] + " --yes"}}
	}
	return []output.NextDo{{Reason: "verify covers none of the configured channels yet; review overall state", Do: "wharfy status"}}
}

// verifyMessage は「何が通り、何が落ち、何を飛ばしたか」を一行にする。
func verifyMessage(checks []verifyCheck, nothingToVerify bool) string {
	byStatus := map[string][]string{}
	for _, ck := range checks {
		byStatus[ck.Status] = append(byStatus[ck.Status], ck.Channel)
	}
	var parts []string
	for _, st := range []string{verifyStatusFailed, verifyStatusOK, verifyStatusPartial, verifyStatusSkipped} {
		if names := byStatus[st]; len(names) > 0 {
			parts = append(parts, st+" "+strings.Join(names, ", "))
		}
	}
	if !nothingToVerify {
		return strings.Join(parts, "; ")
	}
	if len(parts) == 0 {
		return "nothing to verify: no channels in wharfy.yaml"
	}
	return "nothing to verify: " + strings.Join(parts, "; ")
}

// verifyHomebrew は自前 tap の formula が在り、版が記録と一致するかを照合する。
// tap は設定の解決値を先に採る(記録は監査用のフォールバック・D-4)。どちらでも解決できないのは
// 設定の壊れ(検証の失敗ではない)なので error で返す。
func verifyHomebrew(ctx context.Context, cfg config.Config, ch config.ResolvedChannel, tgt verifyTarget) (verifyOutcome, error) {
	tap := firstNonEmptyStr(ch.Target, tgt.Target)
	owner, repo, ok := splitOwnerName(tap)
	if !ok {
		return verifyOutcome{}, errString("homebrew target is unresolved: " + tap)
	}
	hb := &channel.Homebrew{
		Project: cfg.Project,
		Tap:     tap,
		Store:   newTapStore(owner, repo, os.Getenv("GITHUB_TOKEN")),
	}
	rs, perr := hb.Probe(ctx)
	if perr != nil {
		return probeFailedOutcome("homebrew", perr), nil
	}
	switch {
	case !rs.Found:
		return verifyFailure("homebrew",
			"homebrew: no formula at "+tap+" for the expected "+tgt.expected(),
			"the expected version is not on the tap",
			"re-publish to restore the formula",
			"", "wharfy publish homebrew --yes"), nil
	case rs.Version != tgt.Version:
		return verifyFailure("homebrew",
			"tap has "+rs.Version+", expected "+tgt.expected(),
			"the tap formula is not the expected version",
			"re-publish to align the tap with the expected version",
			"", "wharfy publish homebrew --yes"), nil
	default:
		return verifySuccess("homebrew", "homebrew "+rs.Version+" verified: formula present at "+tap+", version matches record"), nil
	}
}

// verifyScoop は自前 bucket の manifest が在り、版が期待と一致するかを照合する(homebrew と同型)。
//
// scoop の消費側は Windows でしか踏めないが、bucket の manifest は HTTP だけで読めるので OS を選ばない
// (D-11: 語る OS の破損を、Linux の CI が先に捕まえる)。manifest の名前は publish と同じ規約で決める
// ——GUI(bundle)は <project>-app、CLI は <project> —— ので、読む先を publish と取り違えない。
func verifyScoop(ctx context.Context, cfg config.Config, in config.File, ch config.ResolvedChannel, tgt verifyTarget) (verifyOutcome, error) {
	bucket := firstNonEmptyStr(ch.Target, tgt.Target)
	owner, repo, ok := splitOwnerName(bucket)
	if !ok {
		return verifyOutcome{}, errString("scoop target is unresolved: " + bucket)
	}
	sc := &channel.Scoop{
		Project: cfg.Project,
		Token:   scoopToken(cfg, in),
		Bucket:  bucket,
		Store:   newTapStore(owner, repo, os.Getenv("GITHUB_TOKEN")),
	}
	rs, perr := sc.Probe(ctx)
	if perr != nil {
		return probeFailedOutcome("scoop", perr), nil
	}
	switch {
	case !rs.Found:
		return verifyFailure("scoop",
			"scoop: no manifest at "+bucket+":"+sc.ManifestPath()+" for the expected "+tgt.expected(),
			"the expected version is not in the bucket",
			"re-publish to restore the manifest",
			"", "wharfy publish scoop --yes"), nil
	case rs.Version != tgt.Version:
		return verifyFailure("scoop",
			"bucket has "+rs.Version+", expected "+tgt.expected(),
			"the bucket manifest is not the expected version",
			"re-publish to align the bucket with the expected version",
			"", "wharfy publish scoop --yes"), nil
	default:
		return verifySuccess("scoop", "scoop "+rs.Version+" verified: manifest present at "+bucket+":"+sc.ManifestPath()+", version matches record"), nil
	}
}

// newReleasesProbe は Release の照合器を組む末端(テストで差し替える)。
var newReleasesProbe = func(owner, repo string) *channel.ReleasesProbe {
	return &channel.ReleasesProbe{Owner: owner, Repo: repo, Token: os.Getenv("GITHUB_TOKEN")}
}

// verifyReleases は Release に「配ったはずの資産」が実在するかを照合する。
//
// 期待集合は wharfy 自身が書く latest.json(在れば GoReleaser の checksums も)。latest.json を
// 持たない Release は failed — release は github(owner/repo)が解決できる限り必ずこれを上げるので、
// 無いのは上げ損ねたか消されたかで、どちらも更新チェックの向き先が 404 になっている。checksums が
// 在れば期待集合は組めてしまうが、それで緑を通すと配布者は壊れたまま気づけない(D-242・D-191 と同じ形)。
//
// 既定は資産本体を落とさない(D-4)ので、ここで捕まえるのは「名前が無い」ことだけ。名前が在っても
// 中身が壊れていることはある(アップロードが途中で切れた・後から差し替えられた)。--install なら
// 資産を落として checksums マニフェストの sha256 と突き合わせ、そこまで見る。
func verifyReleases(ctx context.Context, ch config.ResolvedChannel, tgt verifyTarget) (verifyOutcome, error) {
	repo := firstNonEmptyStr(ch.Target, tgt.Target)
	owner, name, ok := splitOwnerName(repo)
	if !ok {
		return verifyOutcome{}, errString("releases target is unresolved: " + repo)
	}
	probe := newReleasesProbe(owner, name)
	audit, perr := probe.Audit(ctx, tgt.Version)
	if perr != nil {
		return probeFailedOutcome("releases", perr), nil
	}
	switch {
	case !audit.Found:
		return verifyFailure("releases",
			"releases: "+repo+" has no release tagged v"+tgt.Version+" (expected "+tgt.expected()+")",
			"the expected release does not exist",
			"re-run release to cut the tag and upload its assets",
			"", "wharfy release --yes"), nil
	case !audit.HasLatestJSON:
		return verifyFailure("releases",
			"releases: v"+tgt.Version+" carries no "+channel.ManifestLatestJSON,
			"the release has no update manifest, so releases/latest/download/"+channel.ManifestLatestJSON+
				" is a 404 for everyone already running the app",
			"re-run release to upload "+channel.ManifestLatestJSON+" to this tag",
			"", "wharfy release --yes"), nil
	case audit.Version != "" && audit.Version != tgt.Version:
		return verifyFailure("releases",
			channel.ManifestLatestJSON+" on v"+tgt.Version+" says "+audit.Version+", expected "+tgt.Version,
			"the release manifest names a different version than the published record",
			"re-run release so the release and its "+channel.ManifestLatestJSON+" agree",
			"", "wharfy release --yes"), nil
	case len(audit.Missing) > 0:
		return verifyFailure("releases",
			"releases "+tgt.Version+" is missing "+strconv.Itoa(len(audit.Missing))+" of "+
				strconv.Itoa(len(audit.Expected))+" assets listed in "+strings.Join(audit.Manifests, " and "),
			"assets listed in the release manifest are not on the release",
			"re-run release to upload the missing assets; users following the manifest hit a 404",
			strings.Join(audit.Missing, "\n"), "wharfy release --yes"), nil
	}

	present := "releases " + tgt.Version + ": all " + strconv.Itoa(len(audit.Expected)) +
		" assets listed in " + strings.Join(audit.Manifests, " and ") + " exist on the release at " + repo
	if !flagInstall {
		return verifyProbedOnly("releases", present+"; the assets were not downloaded, so their contents are unchecked"), nil
	}
	return verifyReleaseChecksums(ctx, probe, audit, present), nil
}

// verifyReleaseChecksums は --install のときに資産を落として sha256 を検算する。
//
// sha を載せるのは checksums マニフェストだけなので、latest.json しか持たない Release では検算
// できない。それを緑と呼ぶと「中身を確かめた」という嘘になるので partial に落とす —— --install を
// 頼まれて、頼まれた仕事をしていないため(verifyPartial の作法)。
func verifyReleaseChecksums(ctx context.Context, probe *channel.ReleasesProbe, audit channel.ReleaseAudit, present string) verifyOutcome {
	if len(audit.Checksums) == 0 {
		return verifyPartial("releases", present+", but their contents were not checked: "+
			"the release carries no *_"+channel.ManifestChecksums+", so there are no sha256 to compare against")
	}
	cctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()
	bad, err := probe.VerifyChecksums(cctx, audit)
	if err != nil {
		return probeFailedOutcome("releases", err)
	}
	if len(bad) > 0 {
		return verifyFailure("releases",
			strconv.Itoa(len(bad))+" of "+strconv.Itoa(len(audit.Checksums))+" release assets do not match their sha256",
			"the assets on the release differ from what the checksums manifest says they are",
			"re-run release to upload the assets again; users downloading them get something other than what was built",
			mismatchDetail(bad), "wharfy release --yes")
	}
	return verifySuccess("releases", present+", and all "+strconv.Itoa(len(audit.Checksums))+
		" of them match their sha256")
}

func mismatchDetail(bad []channel.ChecksumMismatch) string {
	lines := make([]string, 0, len(bad))
	for _, m := range bad {
		lines = append(lines, m.String())
	}
	return strings.Join(lines, "\n")
}

// verifyScript は公開 install.sh が記録どおりの版を入れるかを確かめる。
//
// 既定は probe: install.sh を取得し、その本文の VERSION="x" を記録と照合する。--install なら
// さらに一時の置き場所へ実際に走らせ、入ったバイナリを起動する ——スクリプトが 404 のアセットを
// 掴んでいても、VERSION の行だけは正しいことがあるので、probe は「入る」ことまでは言えない。
//
// script チャネルは install.sh と install.ps1 の 2 本を配る。probe は HTTP だけで読めるので、
// 走っているホストの OS によらず**両方**を照合する ——さもなければ Linux の CI で verify を回す
// 配布者は、install.ps1 が release から欠けても、古い版を入れる本文でも、緑を受け取る。
// 実際に走らせる(--install)のは、このホストの利用者が踏む一方だけ ——Windows なら install.ps1。
func verifyScript(ctx context.Context, cfg config.Config, in config.File, tgt verifyTarget) verifyOutcome {
	url := scriptProbeURL
	if url == "" {
		url = config.InstallURL(cfg)
	}
	if url == "" {
		return verifySkip("script", "script skipped: the install.sh url is unresolved (set github: owner/repo or script.base_url)")
	}
	version, bad, ok := probeInstaller(ctx, url, config.InstallScriptName, tgt)
	if !ok {
		return bad
	}
	ps1URL := siblingURL(url, config.InstallPS1Name)
	if _, bad, ok := probeInstaller(ctx, ps1URL, config.InstallPS1Name, tgt); !ok {
		return bad
	}
	if !flagInstall {
		return verifyProbedOnly("script", "script "+version+" probed: install.sh and install.ps1 both install "+version+"; neither install was exercised")
	}

	inst := hostScriptInstaller(runtime.GOOS, url)
	out, err := scriptInstall(ctx, inst.URL, prebuiltBinaryName(cfg, in), verifyRun(in))
	switch {
	case errors.Is(err, errToolMissing):
		return verifyPartial("script", "script "+version+" found at "+url+", but the install was not exercised: "+inst.Tool+" is not available")
	case err != nil:
		return verifyFailure("script",
			"script "+version+" is published but installing it failed",
			inst.Name+" failed: "+err.Error(),
			"read the installer output; the release asset it downloads is likely missing or malformed",
			tail(out, 4000), "wharfy release --yes")
	}
	return verifySuccess("script", "script "+version+" verified: installed from "+inst.URL+" into a temporary prefix and ran")
}

// probeInstaller は公開インストーラ 1 本を確かめる —— 在るか、記録どおりの版を入れるか。
// ok=false のとき bad がその理由(probe_failed / failed)を語る。
//
// 版の書き場所は install.sh と install.ps1 で違う。name がそれを決め、読み手を選ぶ ——書式を
// 取り違えると版は空文字になり、記録と一致しないので failed に出る(黙って緑にはならない)。
func probeInstaller(ctx context.Context, url, name string, tgt verifyTarget) (version string, bad verifyOutcome, ok bool) {
	rs, perr := (&channel.Script{InstallURL: url, PS1: name == config.InstallPS1Name}).Probe(ctx)
	switch {
	case perr != nil:
		return "", probeFailedOutcome("script", perr), false
	case !rs.Found:
		return "", verifyFailure("script",
			"script: no "+name+" at "+url+" for the expected "+tgt.expected(),
			"published "+name+" not found",
			"re-run release to upload "+name+" to the release",
			"", "wharfy release --yes"), false
	case rs.Version != tgt.Version:
		return "", verifyFailure("script",
			name+" at "+url+" installs "+rs.Version+", expected "+tgt.expected(),
			"the published "+name+" installs a different version than expected",
			"re-run release so the release and its "+name+" agree",
			"", "wharfy release --yes"), false
	}
	return rs.Version, verifyOutcome{}, true
}

// verifyGoinstall は `go install` が通るかを確かめる。
//
// 発行物を push しないチャネルなので publish 記録が無い。基準は他のチャネルと同じ「確かめる版」で、
// module proxy にその版が在るかを照合する(既定)。--install なら一時 GOBIN へ実際に go install し、
// 起動まで見る。
//
// 版の一致は判定に使わない。go install は wharfy の ldflags を通さないので、版を注入している CLI は
// dev と名乗る ——一致で判定すると偽陰性になる。
func verifyGoinstall(ctx context.Context, cfg config.Config, in config.File, tgt verifyTarget) verifyOutcome {
	mod := channelTargetByName(cfg, "goinstall")
	if mod == "" {
		return verifySkip("goinstall", "goinstall skipped: the module path is unresolved (needs a go.mod)")
	}
	tag := "v" + tgt.Version // module proxy は v 付きのタグで引く
	path := joinModuleMain(mod, cfg.Main)
	rs, perr := (&channel.GoInstall{Module: mod, InstallPath: path, Version: tag, Proxy: goinstallProxy}).Probe(ctx)
	if perr != nil {
		return probeFailedOutcome("goinstall", perr)
	}
	if !rs.Found {
		return verifyFailure("goinstall",
			"the module proxy has no "+mod+"@"+tag+", so `go install` resolves no version",
			"the published tag is not on the module proxy",
			"push the tag and ensure the repo is public; the proxy fetches it on first request",
			"", "git push --tags")
	}
	if !flagInstall {
		return verifyProbedOnly("goinstall", "goinstall "+tag+" probed: the module proxy has "+mod+"@"+tag+"; `go install` was not exercised")
	}

	out, err := goinstallInstall(ctx, path, tag, verifyRun(in))
	switch {
	case errors.Is(err, errToolMissing):
		return verifyPartial("goinstall", "goinstall "+tag+" is on the module proxy, but the install was not exercised: go is not available")
	case err != nil:
		return verifyFailure("goinstall",
			"goinstall "+tag+" is on the module proxy but `go install "+path+"@"+tag+"` failed",
			"go install failed: "+err.Error(),
			"read the output; the module likely does not build at this tag, or "+path+" is not a main package",
			tail(out, 4000), "go install "+path+"@"+tag)
	}
	return verifySuccess("goinstall", "goinstall "+tag+" verified: `go install "+path+"@"+tag+"` installed into a temporary GOBIN and ran")
}

// verifyLinuxRepo は apt/rpm を二段で確かめる。
//
//  1. hosted repo のメタデータに記録どおりの版が載っているか(供給側・既定)。
//  2. その repo を足したコンテナで install し、入ったバイナリが動くか(消費側・--install)。
//
// 2 が無いと、依存不足やパス誤りのパッケージをアップロード成功のまま配ってしまう。だが 2 はイメージ
// の pull と install で数分かかるので、既定では踏まない(D-4: verify の既定は probe)。
func verifyLinuxRepo(ctx context.Context, ch config.ResolvedChannel, cfg config.Config, in config.File, tgt verifyTarget) verifyOutcome {
	name := ch.Name
	repo := firstNonEmptyStr(ch.Target, tgt.Target)
	if repo == "" {
		return verifySkip(name, name+" skipped: repo is unresolved in the config")
	}
	pkg, binary := cfg.Project, prebuiltBinaryName(cfg, in)

	rs, perr := probeLinuxRepo(ctx, name, repo, pkg)
	if perr != nil {
		return probeFailedOutcome(name, perr)
	}
	switch {
	case !rs.Found:
		// 直前に publish したなら、多くは「壊れた」のではなく「まだ載っていない」。索引の生成に
		// 数分かかり、fury 系はそもそも既定で非公開として受け取る(公開に切り替えるまで載らない)。
		// そう言わないと、配布が壊れたと思って無用な調査に時間を溶かす。
		return verifyFailure(name,
			name+": no package at "+repo+" for the expected "+tgt.expected(),
			"published package not found in the repo",
			"if you just published, the repo is likely still indexing (a few minutes); if it never appears, the package may still be private — hosted repos like fury receive uploads as private and only serve them once you flip them to public in the dashboard. otherwise re-publish",
			"", "wharfy publish "+name+" --yes")
	case rs.Version != tgt.Version:
		return verifyFailure(name,
			name+" repo has "+rs.Version+", expected "+tgt.expected(),
			"the repo package is not the expected version",
			"re-publish to align the repo with the expected version",
			"", "wharfy publish "+name+" --yes")
	}

	if !flagInstall {
		return verifyProbedOnly(name, name+" "+rs.Version+" probed: "+repo+" serves "+rs.Version+"; the install was not exercised")
	}
	if !dockerAvailable() {
		return verifyPartial(name, name+" "+rs.Version+" found in "+repo+", but the install was not exercised: docker is not available")
	}
	cv := containerVerify{channel: name, image: verifyImage(in, name), repo: repo, pkg: pkg, binary: binary, run: verifyRun(in)}

	// 設定の書き間違いは検証環境の都合ではないので、コンテナを起こす前に落とす。
	if err := cv.checkShellSafe(); err != nil {
		return verifyFailure(name,
			name+" cannot be verified in a container: "+err.Error(),
			"the verify settings in wharfy.yaml are not usable: "+err.Error(),
			"fix verify.images / verify.run (or the repo url) in wharfy.yaml",
			"", "wharfy verify "+name)
	}

	// イメージを先に引く。引けないのは配布の壊れではなく検証環境の話なので、partial に寄せる
	// (docker 不在と同じ扱い)。ここで分けないと、名指ししたイメージを引けないことが
	// 「パッケージが入らない」に化けて配布者を誤診させる。
	if out, err := cv.pull(ctx); err != nil {
		return verifyPartial(name, name+" "+rs.Version+" found in "+repo+
			", but the install was not exercised: image "+cv.image+" could not be pulled: "+tail(out, 400))
	}
	out, err := cv.install(ctx)
	if err != nil {
		return verifyFailure(name,
			name+" "+rs.Version+" is in the repo but installing it in "+cv.image+" failed",
			"install from the repo failed: "+err.Error(),
			"read the container output; the package's dependencies or file layout are likely wrong",
			tail(out, 4000), "wharfy publish "+name+" --yes")
	}
	return verifySuccess(name, name+" "+rs.Version+" verified: installed from "+repo+" in "+cv.image+" and ran")
}

// verifyImage は apt/rpm を確かめるベースイメージを引く(verify.images > 既定)。
func verifyImage(in config.File, name string) string {
	if in.Verify != nil {
		if img := in.Verify.Images[name]; img != "" {
			return img
		}
	}
	return defaultVerifyImages[name]
}

// verifyRun はコンテナで入れたバイナリに渡す起動確認の引数を引く(空なら既定の連鎖)。
func verifyRun(in config.File) []string {
	if in.Verify == nil {
		return nil
	}
	return in.Verify.Run
}

// probeLinuxRepo は hosted repo のメタデータから pkg の最新版を読む(status と同じ照合器)。
func probeLinuxRepo(ctx context.Context, name, repo, pkg string) (channel.RemoteState, error) {
	if name == "rpm" {
		return (&channel.RpmProbe{Repo: repo}).Probe(ctx, pkg)
	}
	return (&channel.AptProbe{Repo: repo}).Probe(ctx, pkg)
}

// containerVerify は apt/rpm 1 チャネルをコンテナで確かめる 1 回分の材料。
type containerVerify struct {
	channel string   // "apt" / "rpm"
	image   string   // ベースイメージ(verify.images で置き換え可)
	repo    string   // 配信 URL
	pkg     string   // パッケージ名
	binary  string   // 入るはずのバイナリ名
	run     []string // 起動確認の引数(空なら既定の連鎖)
}

// pull はベースイメージを引く。install と分けるのは、引けなかったことを failed ではなく
// partial として扱うため(配布の壊れではない)。
func (cv containerVerify) pull(ctx context.Context) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()
	return dockerRun(cctx, "pull", cv.image)
}

// install は使い捨てコンテナで repo を足し、install → 実行まで走らせる。
// 出力は失敗時の診断に返す(成功しても捨てない)。
func (cv containerVerify) install(ctx context.Context) ([]byte, error) {
	if err := cv.checkShellSafe(); err != nil {
		return nil, err
	}
	script := cv.aptScript()
	if cv.channel == "rpm" {
		script = cv.rpmScript()
	}
	cctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()
	return dockerRun(cctx, "run", "--rm", cv.image, "bash", "-lc", script)
}

// aptScript は debian 系コンテナで repo を足し、install して実行するシェル。
//
// trusted=yes で署名検証を外す。ここで見たいのは「パッケージが入って動くか」であり、
// 鍵を配れているかは repo ホスト側の話なので切り離す。
func (cv containerVerify) aptScript() string {
	return fmt.Sprintf(`set -eu
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq ca-certificates
echo 'deb [trusted=yes] %s /' > /etc/apt/sources.list.d/wharfy-verify.list
apt-get update -qq
apt-get install -y -qq %s
command -v %s
%s
`, cv.repo, cv.pkg, cv.binary, cv.launchCheck())
}

// rpmScript は fedora 系コンテナで repo を足し、install して実行するシェル。
func (cv containerVerify) rpmScript() string {
	return fmt.Sprintf(`set -eu
cat > /etc/yum.repos.d/wharfy-verify.repo <<'EOF'
[wharfy-verify]
name=wharfy verify
baseurl=%s
enabled=1
gpgcheck=0
EOF
dnf install -y -q %s
command -v %s
%s
`, cv.repo, cv.pkg, cv.binary, cv.launchCheck())
}

// launchCheck は入ったバイナリが動くことを確かめる 1 行。
//
// run が空なら --version → version → --help の順に試し、どれか 1 つが通れば「動いた」とみなす
// (サブコマンドの名前はプロジェクトによる。依存不足やパス誤りならどれも起動しない)。
// この推測を受け付けない CLI — サブコマンド必須、引数無しで対話に入る — は verify.run で名指しする。
func (cv containerVerify) launchCheck() string {
	if len(cv.run) == 0 {
		return fmt.Sprintf("%s --version || %s version || %s --help", cv.binary, cv.binary, cv.binary)
	}
	return cv.binary + " " + strings.Join(cv.run, " ")
}

// shellSafeName はコンテナ内のシェルにそのまま渡してよい名前(パッケージ名・バイナリ名)。
var shellSafeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)

// shellSafeArg は起動確認の引数。名前に加えて先頭のハイフン(--version, -h)とサブコマンドを通す。
var shellSafeArg = regexp.MustCompile(`^-{0,2}[A-Za-z0-9][A-Za-z0-9._+-]*$`)

// shellSafeImage はベースイメージの参照(registry/name:tag, @sha256:... まで)。
var shellSafeImage = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]*$`)

// checkShellSafe は設定由来の値をシェルスクリプトへ埋める前に検査する。
// wharfy.yaml は利用者のものだが、書き間違いがコンテナ内で任意コマンドになるのは事故なので断る。
func (cv containerVerify) checkShellSafe() error {
	if !strings.HasPrefix(cv.repo, "http://") && !strings.HasPrefix(cv.repo, "https://") {
		return errString("repo must be an http(s) url to verify in a container: " + cv.repo)
	}
	if strings.ContainsAny(cv.repo, " \t\n\r'\"`$\\") {
		return errString("repo url cannot be passed to a shell safely: " + cv.repo)
	}
	for _, s := range []string{cv.pkg, cv.binary} {
		if !shellSafeName.MatchString(s) {
			return errString("name cannot be passed to a shell safely: " + s)
		}
	}
	for _, a := range cv.run {
		if !shellSafeArg.MatchString(a) {
			return errString("verify.run argument cannot be passed to a shell safely: " + a)
		}
	}
	return checkImageSafe(cv.image)
}

// checkImageSafe はベースイメージ名を docker へ渡す前に検査する。docker はシェルを通さないが、
// 空や `-` 始まりはフラグに化けるし、打ち間違いは早く断る方が診断しやすい。
func checkImageSafe(image string) error {
	if !shellSafeImage.MatchString(image) {
		return errString("verify image is not a usable image reference: " + image)
	}
	return nil
}

// verifySuccess / verifyPartial / verifyFailure / verifySkip / verifyNotRun / probeFailedOutcome は
// 1 チャネル分の結果を組む。
func verifySuccess(name, msg string) verifyOutcome {
	return verifyOutcome{check: verifyCheck{Channel: name, Status: verifyStatusOK, Message: msg}}
}

// verifyPartial は --install を頼まれたのに道具が無くて踏めなかったチャネル。頼まれた仕事をして
// いないので warning に残す(配布者は docker / sh / go を入れれば最後まで確かめられる)。
func verifyPartial(name, msg string) verifyOutcome {
	return verifyOutcome{
		check:   verifyCheck{Channel: name, Status: verifyStatusPartial, Message: msg},
		warning: &output.Warning{Code: output.WarnChannelSkipped, Message: msg},
	}
}

// verifyProbedOnly は既定(--install 無し)の probe で止めたチャネル。実インストールを踏んでいない
// ので verified とは呼ばないが、配布者が選んだ既定どおりに動いただけなので warning は出さない
// ——毎回の verify が warning を吐けば、本当の warning が埋もれる。--install は next で案内する。
func verifyProbedOnly(name, msg string) verifyOutcome {
	return verifyOutcome{check: verifyCheck{Channel: name, Status: verifyStatusPartial, Message: msg}}
}

func verifyFailure(name, msg, problem, hint, detail, next string) verifyOutcome {
	return verifyOutcome{
		check:   verifyCheck{Channel: name, Status: verifyStatusFailed, Message: msg},
		problem: &output.Problem{Code: output.ErrVerifyFailed, Message: problem, Hint: hint, Detail: detail},
		next:    &output.NextDo{Reason: "re-publish " + name + " to fix what verify found", Do: next},
	}
}

// verifySkip は検証を飛ばした事実を warning として残す。ok は落とさないが、黙って通してもいない。
// 配布者が設定で手を打てる skip(repo・install.sh の url・module path が引けない)だけがここを通る。
func verifySkip(name, msg string) verifyOutcome {
	return verifyOutcome{
		check:   verifyCheck{Channel: name, Status: verifyStatusSkipped, Message: msg},
		warning: &output.Warning{Code: output.WarnChannelSkipped, Message: msg},
	}
}

// verifyNotRun は検証を走らせなかったチャネル(未発行・verify が未対応)。warning は出さない
// ——配布者に打てる手が無いか、publish していないという既知の事実だからで、checks には載る。
// 全チャネルがこれなら verifyResult が nothing_to_verify として ok=false にする。
func verifyNotRun(name, msg string) verifyOutcome {
	return verifyOutcome{check: verifyCheck{Channel: name, Status: verifyStatusSkipped, Message: msg}}
}

func probeFailedOutcome(name string, err error) verifyOutcome {
	msg := "cannot reach " + name + " to verify: " + err.Error()
	return verifyOutcome{
		check:   verifyCheck{Channel: name, Status: verifyStatusFailed, Message: msg},
		problem: &output.Problem{Code: output.ErrProbeFailed, Message: err.Error(), Hint: "check network or channel visibility"},
		next:    &output.NextDo{Reason: "retry once reachable", Do: "wharfy verify"},
	}
}

// tail は診断用に出力の末尾 n バイトを返す。
func tail(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[len(b)-n:])
}

type errString string

func (e errString) Error() string { return string(e) }
