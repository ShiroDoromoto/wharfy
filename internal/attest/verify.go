package attest

// verify.go — 預けた来歴が**実際に検算できる**ことを確かめる(証明する側の裏返し)。
//
// 証明は「付けた」で終われない: 署名の形が違う・透明性ログに載っていない・想定と別の workflow が
// 署名した——どれも配る側からは見えず、緑のまま出ていく。捕まえられるのは消費者の目で検算したときだけ
// なので、verify がそれを毎リリース踏む。
//
// 確かめるのは 3 点(どれ 1 つ欠けても来歴は来歴でない):
//   - subject: 証明が**その digest**を指しているか(別のバイト列の証明を持ってきていないか)
//   - 署名者: 想定した repo の workflow が署名したか(誰かの証明を借りていないか)
//   - 透明性ログ: Rekor に載っているか(後から差し替えられない記録が在るか)
//
// 3 点とも BundleVerifier(sigstore-go)が持つ。ここはその呼び出しと、subject ごとの結果の畳み方だけを持つ。

import (
	"context"
	"regexp"
)

// ActionsIssuer は GitHub Actions の OIDC 発行者。keyless 署名の身分はここが配る——
// 別の発行者が名乗る同名の workflow を信じないために、発行者は必ず突き合わせる。
const ActionsIssuer = "https://token.actions.githubusercontent.com"

// Identity は「誰が署名したはずか」。証明書の SAN(署名した workflow の URI)を正規表現で縛る。
type Identity struct {
	Issuer    string
	SANRegexp string
}

// ActionsIdentity は repo(owner/repo)の workflow が署名したことを要求する Identity を作る。
//
// workflow のファイル名までは縛らない: 配布の workflow 名は配布者の自由で、wharfy が決めるものではない。
// 縛るのは「そのリポジトリの workflow であること」——他人のリポジトリで作られた証明は弾かれる。
func ActionsIdentity(repo string) Identity {
	return Identity{
		Issuer:    ActionsIssuer,
		SANRegexp: `^https://github\.com/` + regexp.QuoteMeta(repo) + `/\.github/workflows/`,
	}
}

// Fetcher は digest に預けてある来歴の取り口(GitHub attestations API)。テストは差し替える。
// 証明が 1 つも無ければ空を返す(エラーではない——「付いていない」は正常に観測できる事実)。
type Fetcher interface {
	Bundles(ctx context.Context, sha256 string) ([][]byte, error)
}

// BundleVerifier は bundle 1 つを検算する末端(sigstore-go)。テストは差し替える。
// 上の 3 点すべてを見て、1 つでも欠ければ error を返す。
type BundleVerifier interface {
	VerifyBundle(ctx context.Context, bundleJSON []byte, sha256 string, id Identity) error
}

// Checked は subject 1 つの検算結果。
//
// Attested=false は 2 通りある: 証明がそもそも無い(Err=nil)か、在るが検算できない(Err≠nil)。
// 前者は「まだ付けていない」、後者は「付いているつもりで壊れている」——読み手にとって別の事件なので、
// 畳まずに区別する。
type Checked struct {
	Subject  Subject
	Attested bool
	Err      error
}

// Coverage は subjects 全体の検算結果。
type Coverage struct {
	Checked []Checked
}

// Verified は全 subject の来歴が検算できたか(subjects が空なら false——確かめた物が無い)。
func (c Coverage) Verified() bool {
	return len(c.Checked) > 0 && len(c.Missing()) == 0 && len(c.Broken()) == 0
}

// Missing は証明が預けられていない subject。
func (c Coverage) Missing() []Checked {
	var out []Checked
	for _, ck := range c.Checked {
		if !ck.Attested && ck.Err == nil {
			out = append(out, ck)
		}
	}
	return out
}

// Broken は証明は在るのに検算できなかった subject。
func (c Coverage) Broken() []Checked {
	var out []Checked
	for _, ck := range c.Checked {
		if ck.Err != nil {
			out = append(out, ck)
		}
	}
	return out
}

// Verify は subjects 1 つずつの来歴を検算する。
//
// subject ごとに引くのが要点: 1 つの証言に全 subject を載せていても、載せ損ねた 1 つは「その digest では
// 何も引けない」形で現れる。まとめて 1 回引くと、その取りこぼしが見えない。
//
// 取り口そのものが落ちた(ネットワーク・API)ときは error を返す——「証明が無い」と取り違えないため。
func Verify(ctx context.Context, subjects []Subject, id Identity, f Fetcher, v BundleVerifier) (Coverage, error) {
	var cov Coverage
	for _, s := range subjects {
		bundles, err := f.Bundles(ctx, s.SHA256)
		if err != nil {
			return Coverage{}, err
		}
		cov.Checked = append(cov.Checked, check(ctx, s, bundles, id, v))
	}
	return cov, nil
}

// check は 1 subject に預けてある bundle を検算する。
//
// 1 つでも通れば来歴は在る: 同じ digest に複数の証明が預けられることはある(再リリース、別の証言)。
// 通るものが 1 つも無ければ、最後の失敗理由を返す——「在るのに検算できない」を黙らせない。
func check(ctx context.Context, s Subject, bundles [][]byte, id Identity, v BundleVerifier) Checked {
	if len(bundles) == 0 {
		return Checked{Subject: s}
	}
	var last error
	for _, b := range bundles {
		if err := v.VerifyBundle(ctx, b, s.SHA256, id); err != nil {
			last = err
			continue
		}
		return Checked{Subject: s, Attested: true}
	}
	return Checked{Subject: s, Err: last}
}
