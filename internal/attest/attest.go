// Package attest はビルドの来歴(provenance)を証明する段。
//
// 配るものを公開 CI でビルドするようにしても(release.yml)、CI が作った物が本当にその workflow・
// その commit から出たものかを、受け取る側が確かめる手立ては無い。attest はそれを与える:
// 配布物の digest を subject にした in-toto の証言を Sigstore で keyless 署名し、GitHub の
// attestations API に預ける。受け取る側は `gh attestation verify` でそれを検算できる。
//
// 責務境界:
//   - sign(codesign)は**配布物そのものの署名**、attest は**ビルドの来歴**。別物だが、
//     「opt-in・環境に資格情報があるときだけ働き、無ければ素通し」という振る舞いは同型。
//   - 証明できるのは OIDC を配る場所(GitHub Actions)だけ。手元には OIDC が無いので自然に no-op に
//     なる——黙って何もしないのではなく、そうと分かる Reason を出す(Status)。
//   - 証明が付く範囲は正直に出す(Coverage)。「全チャネルに来歴が付く」は嘘になる。
//
// 依存方向: ドメイン層なので上位(output/emit・CLI・config)を import しない。env の読み取りは
// CLI 層が行い、解決済みの Options だけを受け取る(sign と同じ作法)。
package attest

import "context"

// Subject は来歴を証明する対象 1 つ。GitHub は digest で証明を引くので、名前は人間向けの札に過ぎず、
// 同一性を担うのは sha256 の方(どのチャネル経由で受け取っても、バイト列が同じなら証明に当たる)。
type Subject struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

// Options は解決済みの attest 設定(CLI 層が env から組み立てて渡す)。
//   - Repo: 証明を預ける先の owner/repo。attestations API はリポジトリに紐づく。
//   - Token: GITHUB_TOKEN(workflow の permissions に attestations: write が要る)。
//   - OIDC: keyless 署名に使う OIDC の取り口。Actions が env で渡す(id-token: write が要る)。
//   - Env: 来歴そのものに載せる GitHub Actions の文脈(どの workflow のどの run が作ったか)。
type Options struct {
	Repo  string
	Token string
	OIDC  OIDCEnv
	Env   Env
}

// Enabled は attest を実行できるかを返す。3 つ揃って初めて意味を持つ:
// OIDC が無ければ誰の名でも署名できず、token が無ければ預けられず、repo が無ければ預け先が無い。
func (o Options) Enabled() bool {
	return o.OIDC.Available() && o.Token != "" && o.Repo != ""
}

// State は「いま来歴を証明できるか」の状態(status が出す)。
type State struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
	// Covered / Uncovered は証明が及ぶ範囲/及ばない範囲を 1 行 1 点で述べる。
	Covered   []string `json:"covered,omitempty"`
	Uncovered []string `json:"uncovered,omitempty"`
}

// 証明が及ぶ範囲。GitHub の証明は subject の digest で引く——だから「どこから落としたか」ではなく
// 「バイト列が同じか」で当たる。tap も bucket も AUR も fury も、Release のアセットと同じファイルを
// 配っている限り、その利用者は同じ証明を検算できる。
var coveredLines = []string{
	"the build artifacts uploaded to the github release (archives, packages, bundles), by sha256 digest",
	"attestations are looked up by digest, not by host: a channel that serves those exact files (tap, bucket, scoop, aur, your apt/rpm repo) carries the same provenance — `gh attestation verify <file> --repo <owner>/<repo>` proves it wherever the file came from",
}

// 証明が及ばない範囲。ここを黙ると「全部に来歴が付く」と読まれる。
var uncoveredLines = []string{
	"the container image: its digest is a separate subject and is not attested yet",
	"homebrew-core: it rebuilds from source and bottles the result, so what users install is not the bytes wharfy signed",
	"install.sh / install.ps1 / latest.json: they are release assets but not build outputs, and are not attested yet",
}

// Status は attest の現状を述べる。実行できないなら「なぜできないか」を必ず持たせる——
// 手元では OIDC が無いので常に no-op になるが、それは異常ではなく前提なので、そう言い切る。
func Status(opts Options) State {
	st := State{Covered: coveredLines, Uncovered: uncoveredLines}
	switch {
	case !opts.OIDC.Available():
		st.Reason = "no OIDC token available — provenance is signed with the identity GitHub Actions hands the workflow, so nothing is attested from a laptop (in CI: permissions: id-token: write)"
	case opts.Token == "":
		st.Reason = "no GITHUB_TOKEN — the attestation is stored on the repository (in CI: permissions: attestations: write)"
	case opts.Repo == "":
		st.Reason = "cannot resolve the github owner/repo to store the attestation on"
	default:
		st.Available = true
	}
	return st
}

// Result は 1 回の attest の結果(release の報告用)。
type Result struct {
	Subjects []Subject `json:"subjects,omitempty"`
	// ID は GitHub が採番した attestation の id(預けた証拠)。
	ID int64 `json:"id,omitempty"`
}

// Signer は in-toto の証言(DSSE payload)を Sigstore bundle に署名する末端。
// 実体は sigstore-go(SigstoreSigner)。テストは差し替える。
type Signer interface {
	SignDSSE(ctx context.Context, statement []byte, idToken string) ([]byte, error)
}

// TokenSource は keyless 署名に使う OIDC トークンの取り口(Actions の env 経由)。テストは差し替える。
type TokenSource interface {
	IDToken(ctx context.Context, audience string) (string, error)
}

// Store は署名済み bundle の預け先(GitHub attestations API)。テストは差し替える。
type Store interface {
	Put(ctx context.Context, bundleJSON []byte) (int64, error)
}

// sigstoreAudience は Fulcio が受け取る OIDC の宛先。公開 Sigstore の約束事。
const sigstoreAudience = "sigstore"

// Error は attest 段の失敗(証明できる環境で証明に失敗した)。上位はこの型で分類して
// attest_failed を返す——ビルド失敗にも内部エラーにも紛れさせない(sign と同じ作法)。
type Error struct{ Err error }

func (e *Error) Error() string { return "attest: " + e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

// Attest は subjects の来歴を証明して預ける。
//
// 順序が意味を持つ: 証言(statement)は**配ったバイト列の digest**で作る。だから release は
// アセットを上げ切った後——実 sha256 が確定した後——にここを呼ぶ。
// subjects が空なら何もしない(証明する物が無い)。
func Attest(ctx context.Context, opts Options, subjects []Subject, tokens TokenSource, signer Signer, store Store) (Result, error) {
	if len(subjects) == 0 {
		return Result{}, nil
	}
	idToken, err := tokens.IDToken(ctx, sigstoreAudience)
	if err != nil {
		return Result{}, &Error{Err: err}
	}
	statement, err := Statement(subjects, opts.Env)
	if err != nil {
		return Result{}, &Error{Err: err}
	}
	bundle, err := signer.SignDSSE(ctx, statement, idToken)
	if err != nil {
		return Result{}, &Error{Err: err}
	}
	id, err := store.Put(ctx, bundle)
	if err != nil {
		return Result{}, &Error{Err: err}
	}
	return Result{Subjects: subjects, ID: id}, nil
}
