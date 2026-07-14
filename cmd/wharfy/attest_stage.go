package main

// attest_stage.go — attest 段の CLI 側オーケストレーション。env から設定を解決し、配った物の
// digest に来歴を付ける。
//
// 段の位置が要点: 証言は**配ったバイト列**の digest で作るので、実 digest が確定した後にしか作れない。
// 証明する物は 2 つあり、digest が決まる時点が違うので、段も 2 つに分かれる:
//
//	Release のアセット —— 手元のファイルを数えれば digest が出る。sign もアーカイブも終わった後、
//	                      release の最後で付ける(sign 段が「archive の前」なのと対になる)。
//	container の image —— 同一性(manifest digest)を決めるのは push を受けたレジストリの方。
//	                      だから release では作れず、image を配った直後(publish container)に付ける。
//
// 働くのは GitHub Actions の中だけ(OIDC を配るのはそこだけ)。手元では素通しし、CI で証明を作れない
// ときだけ警告する——「証明は無くても配れてしまう」ので、黙ると付いているつもりのまま配り続ける。

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/ShiroDoromoto/wharfy/internal/attest"
	"github.com/ShiroDoromoto/wharfy/internal/build"
	"github.com/ShiroDoromoto/wharfy/internal/channel"
	"github.com/ShiroDoromoto/wharfy/internal/config"
	"github.com/ShiroDoromoto/wharfy/internal/output"
)

// Actions が置く env(資格情報は環境から取る、という wharfy の作法のまま)。
const (
	envOIDCRequestURL   = "ACTIONS_ID_TOKEN_REQUEST_URL"
	envOIDCRequestToken = "ACTIONS_ID_TOKEN_REQUEST_TOKEN"
	envGitHubActions    = "GITHUB_ACTIONS"
)

// 生成点(テストで差し替える＝末端は差し替え可能。sign の newSigner と同じ作法)。
var (
	newAttestSigner = func() attest.Signer { return attest.NewSigner() }
	newAttestTokens = func(env attest.OIDCEnv) attest.TokenSource { return attest.ActionsTokens{Env: env} }
	newAttestStore  = func(owner, repo, token string) attest.Store { return attest.NewGitHubStore(owner, repo, token) }
)

// resolveAttestOptions は env(と解決済み config)から attest 設定を組み立てる。
func resolveAttestOptions(cfg config.Config) attest.Options {
	repo := os.Getenv("GITHUB_REPOSITORY")
	if repo == "" {
		// CI の外(env が無い)なら wharfy.yaml の github: を採る。証明を預ける先は同じリポジトリ。
		if owner, name, ok := splitOwnerName(cfg.Github); ok {
			repo = owner + "/" + name
		}
	}
	return attest.Options{
		Repo:  repo,
		Token: os.Getenv("GITHUB_TOKEN"),
		OIDC: attest.OIDCEnv{
			RequestURL:   os.Getenv(envOIDCRequestURL),
			RequestToken: os.Getenv(envOIDCRequestToken),
		},
		Env: attest.Env{
			Repository:      repo,
			RepositoryID:    os.Getenv("GITHUB_REPOSITORY_ID"),
			RepositoryOwner: os.Getenv("GITHUB_REPOSITORY_OWNER_ID"),
			ServerURL:       os.Getenv("GITHUB_SERVER_URL"),
			SHA:             os.Getenv("GITHUB_SHA"),
			Ref:             os.Getenv("GITHUB_REF"),
			WorkflowRef:     os.Getenv("GITHUB_WORKFLOW_REF"),
			EventName:       os.Getenv("GITHUB_EVENT_NAME"),
			RunID:           os.Getenv("GITHUB_RUN_ID"),
			RunAttempt:      os.Getenv("GITHUB_RUN_ATTEMPT"),
			RunnerEnv:       os.Getenv("RUNNER_ENVIRONMENT"),
		},
	}
}

// attestSubjects は release が上げた成果物を証言の subject にする。
// sha256 は release が実ファイルから確定した値(宣言値ではない)。持たない成果物は証明できないので落とす
// ——digest の無い subject は、検算する側から見て何も指していない。
func attestSubjects(archs []build.Artifact) []attest.Subject {
	subs := make([]attest.Subject, 0, len(archs))
	for _, a := range archs {
		if a.SHA256 == "" {
			continue
		}
		subs = append(subs, attest.Subject{Name: filepath.Base(a.Path), SHA256: a.SHA256})
	}
	return subs
}

// extraSubjects は release が上げた「ビルド出力ではない資産」を subject にする。
//
// install.sh は利用者が `curl | sh` で**実行する**もので、供給網としてはアーカイブより重い。
// ビルドが作った物ではないという理由でここを証明の外に置くと、一番踏まれる経路が一番無防備になる。
// latest.json も同じで、更新チェックの向き先を書き換えられたら利用者はそちらへ流れる。
//
// 読むのは上げたファイルそのもの(.wharfy 配下)。sha256 はここで数える——宣言値は使わない。
func extraSubjects(paths []string) ([]attest.Subject, error) {
	subs := make([]attest.Subject, 0, len(paths))
	for _, p := range paths {
		sum, err := build.SHA256File(p)
		if err != nil {
			return nil, err
		}
		subs = append(subs, attest.Subject{Name: filepath.Base(p), SHA256: sum})
	}
	return subs, nil
}

// releaseExtras は release がこのタグへ上げた「ビルド出力ではない資産」のパスを返す。
//
// 経路(Go/BYO)で上げ方は違う——Go は goreleaser の extra_files、BYO は wharfy 自身のアップロード
// ——が、書く先は同じ .wharfy 配下で、上げたか否かの条件も同じ(script チャネルが配る版を持つか)。
// その 1 点に寄せることで、経路によって証明の有無が変わる状態を作らない。
//
// latestPath は uploadLatestJSON が実際に上げた latest.json(上げていなければ空)。上げていない物を
// subject にすると、Release に存在しない digest の証明を作ることになる。
func releaseExtras(root string, cfg config.Config, version, latestPath string) []string {
	var paths []string
	if latestPath != "" {
		paths = append(paths, latestPath)
	}
	if _, ship := installScriptTarget(root, cfg, version); config.HasChannel(cfg, "script") && ship {
		paths = append(paths,
			filepath.Join(root, config.InstallScriptRelPath),
			filepath.Join(root, config.InstallPS1RelPath))
	}
	return paths
}

// attestRelease は release が上げた物(成果物＋ extras)に来歴を付ける。
//
// extras は install.sh / install.ps1 / latest.json のような「ビルド出力ではないが release が配る物」
// のパス。証明は**配ったバイト列**に付くので、上げた物は等しく subject にする。
//
// 返り値の warning は「CI なのに証明できなかった」= 配布者が気づくべき欠落。error は
// 「証明できる環境で失敗した」= release ごと赤くする(ErrAttestFailed)。
// 手元(OIDC 無し)では何も返さない——それは異常ではなく前提。
func attestRelease(ctx context.Context, cfg config.Config, archs []build.Artifact, extras []string) (*attest.Result, *output.Warning, error) {
	opts := resolveAttestOptions(cfg)
	if !opts.Enabled() {
		if os.Getenv(envGitHubActions) != "true" {
			return nil, nil, nil // 手元: 証明できないのが前提(status がそう言っている)
		}
		return nil, &output.Warning{
			Code:    output.WarnAttestUnavailable,
			Message: "no build provenance was attached: " + attest.Status(opts).Reason,
		}, nil
	}
	subjects := attestSubjects(archs)
	extraSubs, err := extraSubjects(extras)
	if err != nil {
		return nil, nil, &attest.Error{Err: err}
	}
	subjects = append(subjects, extraSubs...)
	res, err := attestSubjectsWith(ctx, opts, subjects)
	return res, nil, err
}

// attestContainerImage は push 済みの image:version の manifest digest に来歴を付ける。
//
// release で付けられないのは、image の同一性を決めるのが wharfy ではなくレジストリだから: tag は
// 動かせるので image を名指せるのは digest だけで、その digest は push を受けたレジストリが返して
// 初めて分かる。だから証明は container を配った直後——ここ——でしか作れない。
//
// 引くときはアセットと同じ(digest で引く): 利用者は
// `gh attestation verify oci://<image>:<version> --repo <owner>/<repo>` で検算できる。
//
// image がレジストリに無いなら何もしない——配っていない物の来歴は無くて当然で、image の不在は
// container の行が言うことだから(attest が二重に言わない)。
func attestContainerImage(ctx context.Context, cfg config.Config, image, version string) (*attest.Result, *output.Warning, error) {
	if image == "" || version == "" {
		return nil, nil, nil
	}
	opts := resolveAttestOptions(cfg)
	if !opts.Enabled() {
		if os.Getenv(envGitHubActions) != "true" {
			return nil, nil, nil // 手元: 証明できないのが前提(status がそう言っている)
		}
		return nil, &output.Warning{
			Code:    output.WarnAttestUnavailable,
			Message: "no build provenance was attached to the container image: " + attest.Status(opts).Reason,
		}, nil
	}
	sub, found, err := imageSubject(ctx, image, version)
	if err != nil {
		return nil, nil, &attest.Error{Err: err}
	}
	if !found {
		return nil, nil, nil
	}
	if sub.SHA256 == "" {
		// tag は在るのに digest を返さないレジストリ。証明の subject を推測で作らない
		// (指す先の違う証明は、無いより悪い)。付かなかったことは言う。
		return nil, &output.Warning{
			Code: output.WarnAttestUnavailable,
			Message: "no build provenance was attached to " + image + ":" + version +
				": the registry served no sha256 digest for the image, and provenance is bound to a digest",
		}, nil
	}
	res, err := attestSubjectsWith(ctx, opts, []attest.Subject{sub})
	return res, nil, err
}

// imageSubject はレジストリが返す image:version の manifest digest を subject にする。
// found は tag の実在、SHA256 が空なら digest を得られなかった(取り違えないよう分けて返す)。
func imageSubject(ctx context.Context, image, version string) (attest.Subject, bool, error) {
	probe := &channel.OCIProbe{Image: image, Token: os.Getenv("GITHUB_TOKEN"), Base: ociProbeBase}
	digest, found, err := probe.Digest(ctx, version)
	if err != nil || !found {
		return attest.Subject{}, false, err
	}
	hex, ok := strings.CutPrefix(digest, "sha256:")
	if !ok {
		return attest.Subject{Name: image}, true, nil
	}
	return attest.Subject{Name: image, SHA256: hex}, true, nil
}

// attestSubjectsWith は subjects に来歴を付けて預ける(証明できる環境であることは呼び手が確かめた後)。
// subject が無い・預け先を組めないなら何もしない——証明する物も、預ける先も無い。
func attestSubjectsWith(ctx context.Context, opts attest.Options, subjects []attest.Subject) (*attest.Result, error) {
	if len(subjects) == 0 {
		return nil, nil
	}
	owner, name, ok := splitOwnerName(opts.Repo)
	if !ok {
		return nil, nil
	}
	res, err := attest.Attest(ctx, opts, subjects,
		newAttestTokens(opts.OIDC), newAttestSigner(), newAttestStore(owner, name, opts.Token))
	if err != nil {
		return nil, err
	}
	return &res, nil
}
