// Package sign は成果物署名の段。
//
// 当初 MVP の sign は「状態の読み手＋案内役」で実署名しない no-op だった。依頼①でこれを
// **実署名できる opt-in の一段**に昇格させる: 利用者が指定した外部 identity(キーチェーン上の
// 証明書名 or 可搬 PKCS#12)で、ステージ済みの macOS Mach-O バイナリを署名する。
//
// 責務境界:
//   - 署名する/しないは opt-in。identity が解決できなければ従来どおり素通し(pre-signed 持ち込みを尊重)。
//   - notarize は必須にしない(自己署名で完結できる)。将来オプションで足す余地は残す。
//   - 署名でハッシュが変わるため、**署名 → checksum 確定 → release/publish** の順は上位(release)が守る。
//   - Windows Authenticode / bundle 再署名は将来(本 MVP は macOS の prebuilt バイナリのみ)。
//
// 依存方向: ドメイン層なので上位(output/emit・CLI・config)を import しない。設定の解決(yaml/env の
// 読み取り)は CLI 層が行い、解決済みの Options だけを受け取る。
package sign

// Options は解決済みの署名設定(CLI 層が yaml/env から組み立てて渡す)。
//   - Identity: codesign の --sign 引数(キーチェーン上の証明書名)。空なら署名しない(no-op)。
//   - P12: 可搬 PKCS#12 のパス。空ならキーチェーン常駐の Identity を使う。指定時は一時キーチェーンへ
//     import してから署名する(CI 等・証明書がキーチェーンに無い環境向け)。
//   - P12Pass: P12 のパスワード(env 由来)。**出力にもログにも出さない**。
type Options struct {
	Identity string
	P12      string
	P12Pass  string
}

// Enabled は署名を実行するか(identity が解決できているか)を返す。
// identity が無ければ何をどう import しても codesign は誰の名で署名するか決められないので false。
func (o Options) Enabled() bool { return o.Identity != "" }

// Target は OS ごとの署名/公証状態(schemas/common.json signTarget と同形)。
type Target struct {
	Signed    bool   `json:"signed"`
	Notarized bool   `json:"notarized,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// Status は「まだ署名を実行していない」ときの advisory 状態を返す(未 prebuilt / identity 未指定 /
// 非 macOS ホスト等)。実際に署名した結果は CLI 層が Target{Signed:true} を組んで表現する。
// linux は OS レベル署名が不要なのでエントリを出さない。キーは OS 名。
func Status(goos []string, opts Options) map[string]Target {
	out := map[string]Target{}
	for _, os := range goos {
		switch os {
		case "darwin":
			if opts.Enabled() {
				out["darwin"] = Target{Signed: false, Reason: "identity configured (" + opts.Identity + ") — run `wharfy release` to sign and cut checksums"}
			} else {
				out["darwin"] = Target{Signed: false, Reason: "no signing identity configured (set sign.identity or WHARFY_SIGN_IDENTITY); pre-signed binaries are respected"}
			}
		case "windows":
			out["windows"] = Target{Signed: false, Reason: "no code-signing certificate configured (Authenticode signing not yet supported)"}
		}
	}
	return out
}
