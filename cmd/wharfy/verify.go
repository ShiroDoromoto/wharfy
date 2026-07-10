package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
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
// homebrew は自前 tap の formula の有無と版を照合する。releases は Release の資産マニフェスト
// (latest.json / checksums.txt)が載せる資産が実在するかを照合する(本体は落とさない・D-4)。
// apt/rpm はそれらに加えて Linux コンテナで repo を足し、install して実行まで踏む。供給側
// (hosted repo への push)はアップロードが 200 を返せば成功するので、生成した deb/rpm の依存や
// ファイル配置が壊れていても気づけない。踏むのは利用者になる。
//
// `wharfy verify [channel]` は対象を 1 チャネルに絞る。channels: に無い名前は publish と同じ
// channel_not_configured で拒む。
//
// docker が無ければ apt/rpm のコンテナ検証だけを skip する(docker 不在は verify の失敗ではない)。
// 一つも検証できなかったときは ok=false(nothing_to_verify)。「確かめられなかった」を緑で返すと、
// CI がそれを「配布は健全」と読んでしまう(D-4)。

// verifyCheck は 1 チャネル分の検証結果(verify の data)。
type verifyCheck struct {
	Channel string `json:"channel"`
	Status  string `json:"status"` // verified / partial / failed / skipped
	Message string `json:"message"`
}

// verifyData は verify の data ペイロード。
type verifyData struct {
	Checks []verifyCheck `json:"checks"`
}

const (
	verifyStatusOK = "verified"
	// verifyStatusPartial は「検証は走ったが、最後まで踏めなかった」(例: repo の版は照合したが
	// docker が無く install を試せない)。失敗ではないので ok は落とさないが、verified とも呼ばない。
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
	// verifyTimeout はコンテナ 1 本の上限。apt-get update / dnf のメタデータ取得は遅い。
	verifyTimeout = 10 * time.Minute
)

// runVerify は channels: にあるチャネルの到達性・整合性を確認する(verify)。
// 引数でチャネルを1つ名指しできる(省略時は channels: の全部)。apt/rpm はコンテナを起こすので、
// 1 チャネルを直している間は他を走らせない方が反復が軽い。
// 未発行・未対応のチャネルは skip として checks に載せ、一つも検証できなければ ok=false を返す。
func runVerify(ctx context.Context, c registry.Command, args []string) output.Result {
	root, err := os.Getwd()
	if err != nil {
		return internalError(c, err)
	}
	in, _ := config.Load(root)
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

	var outcomes []verifyOutcome
	var unpublished []string
	for _, ch := range targets {
		rec, published := publishedRecord(st, ch.Name)
		if !published {
			unpublished = append(unpublished, ch.Name)
			outcomes = append(outcomes, verifyNotRun(ch.Name, ch.Name+" skipped: nothing published on this channel yet"))
			continue
		}
		switch ch.Name {
		case "homebrew":
			oc, err := verifyHomebrew(ctx, cfg, ch, rec)
			if err != nil {
				return internalError(c, err)
			}
			outcomes = append(outcomes, oc)
		case "releases":
			oc, err := verifyReleases(ctx, ch, rec)
			if err != nil {
				return internalError(c, err)
			}
			outcomes = append(outcomes, oc)
		case "apt", "rpm":
			outcomes = append(outcomes, verifyLinuxRepo(ctx, ch, cfg, in, rec))
		default:
			outcomes = append(outcomes, verifyNotRun(ch.Name, ch.Name+" skipped: verify does not cover this channel yet"))
		}
	}
	return verifyResult(c, outcomes, unpublished)
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
func verifyResult(c registry.Command, outcomes []verifyOutcome, unpublished []string) output.Result {
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
	res.Data = verifyData{Checks: checks}
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
		res.Next = []output.NextDo{{Reason: "distribution looks consistent; review overall state", Do: "wharfy status"}}
	}
	return res
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
func verifyHomebrew(ctx context.Context, cfg config.Config, ch config.ResolvedChannel, rec state.PublishRecord) (verifyOutcome, error) {
	tap := firstNonEmptyStr(ch.Target, rec.Target)
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
			"homebrew recorded "+rec.Version+" but no formula at "+tap,
			"published formula not found on the tap",
			"re-publish to restore the formula",
			"", "wharfy publish homebrew --yes"), nil
	case rs.Version != rec.Version:
		return verifyFailure("homebrew",
			"tap has "+rs.Version+", expected "+rec.Version,
			"tap formula version does not match the published record",
			"re-publish to align the tap with the recorded version",
			"", "wharfy publish homebrew --yes"), nil
	default:
		return verifySuccess("homebrew", "homebrew "+rs.Version+" verified: formula present at "+tap+", version matches record"), nil
	}
}

// newReleasesProbe は Release の照合器を組む末端(テストで差し替える)。
var newReleasesProbe = func(owner, repo string) *channel.ReleasesProbe {
	return &channel.ReleasesProbe{Owner: owner, Repo: repo, Token: os.Getenv("GITHUB_TOKEN")}
}

// verifyReleases は Release に「配ったはずの資産」が実在するかを照合する。
//
// 期待集合は wharfy 自身が書く latest.json(在れば checksums.txt も)。どちらも無い旧リリースは
// 照合の基準を持たないので skip する — 資産の欠落を見ていないのに verified とは言わない(D-4)。
// 資産本体は落とさない(D-4)ので、ここで捕まえるのは「名前が無い」ことだけ。
func verifyReleases(ctx context.Context, ch config.ResolvedChannel, rec state.PublishRecord) (verifyOutcome, error) {
	repo := firstNonEmptyStr(ch.Target, rec.Target)
	owner, name, ok := splitOwnerName(repo)
	if !ok {
		return verifyOutcome{}, errString("releases target is unresolved: " + repo)
	}
	audit, perr := newReleasesProbe(owner, name).Audit(ctx, rec.Version)
	if perr != nil {
		return probeFailedOutcome("releases", perr), nil
	}
	switch {
	case !audit.Found:
		return verifyFailure("releases",
			"releases recorded "+rec.Version+" but "+repo+" has no release tagged v"+rec.Version,
			"published release not found",
			"re-run release to cut the tag and upload its assets",
			"", "wharfy release --yes"), nil
	case len(audit.Manifests) == 0:
		return verifySkip("releases",
			"releases skipped: v"+rec.Version+" carries neither "+channel.ManifestLatestJSON+
				" nor "+channel.ManifestChecksums+", so the expected assets cannot be established"), nil
	case audit.Version != "" && audit.Version != rec.Version:
		return verifyFailure("releases",
			channel.ManifestLatestJSON+" on v"+rec.Version+" says "+audit.Version+", expected "+rec.Version,
			"the release manifest names a different version than the published record",
			"re-run release so the release and its "+channel.ManifestLatestJSON+" agree",
			"", "wharfy release --yes"), nil
	case len(audit.Missing) > 0:
		return verifyFailure("releases",
			"releases "+rec.Version+" is missing "+strconv.Itoa(len(audit.Missing))+" of "+
				strconv.Itoa(len(audit.Expected))+" assets listed in "+strings.Join(audit.Manifests, " and "),
			"assets listed in the release manifest are not on the release",
			"re-run release to upload the missing assets; users following the manifest hit a 404",
			strings.Join(audit.Missing, "\n"), "wharfy release --yes"), nil
	default:
		return verifySuccess("releases", "releases "+rec.Version+" verified: all "+strconv.Itoa(len(audit.Expected))+
			" assets listed in "+strings.Join(audit.Manifests, " and ")+" exist on the release at "+repo), nil
	}
}

// verifyLinuxRepo は apt/rpm を二段で確かめる。
//
//  1. hosted repo のメタデータに記録どおりの版が載っているか(供給側)。
//  2. その repo を足したコンテナで install し、入ったバイナリが動くか(消費側)。
//
// 2 が無いと、依存不足やパス誤りのパッケージをアップロード成功のまま配ってしまう。
func verifyLinuxRepo(ctx context.Context, ch config.ResolvedChannel, cfg config.Config, in config.File, rec state.PublishRecord) verifyOutcome {
	name := ch.Name
	repo := firstNonEmptyStr(ch.Target, rec.Target)
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
		return verifyFailure(name,
			name+" recorded "+rec.Version+" but no package at "+repo,
			"published package not found in the repo",
			"re-publish to restore the package",
			"", "wharfy publish "+name+" --yes")
	case rs.Version != rec.Version:
		return verifyFailure(name,
			name+" repo has "+rs.Version+", expected "+rec.Version,
			"repo package version does not match the published record",
			"re-publish to align the repo with the recorded version",
			"", "wharfy publish "+name+" --yes")
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

// verifyPartial は検証の一部だけを踏めたチャネル。踏めなかった事実を warning に残す
// (配布者は docker を入れれば最後まで確かめられる)。
func verifyPartial(name, msg string) verifyOutcome {
	return verifyOutcome{
		check:   verifyCheck{Channel: name, Status: verifyStatusPartial, Message: msg},
		warning: &output.Warning{Code: output.WarnChannelSkipped, Message: msg},
	}
}

func verifyFailure(name, msg, problem, hint, detail, next string) verifyOutcome {
	return verifyOutcome{
		check:   verifyCheck{Channel: name, Status: verifyStatusFailed, Message: msg},
		problem: &output.Problem{Code: output.ErrVerifyFailed, Message: problem, Hint: hint, Detail: detail},
		next:    &output.NextDo{Reason: "re-publish " + name + " to fix what verify found", Do: next},
	}
}

// verifySkip は検証を飛ばした事実を warning として残す。ok は落とさないが、黙って通してもいない。
// 配布者が手を打てる skip(docker を入れる・repo を設定する)だけがここを通る。
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
