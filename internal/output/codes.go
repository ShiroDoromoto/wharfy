package output

// codes.go — warning / error コードの正準カタログ。
//
// 規約:
//   - コードは snake_case の安定識別子。**追加は非破壊**、**改名・意味変更は破壊的**
//     (schema_version と同じ扱い)。
//   - message は人間向け(変わってよい)。**分岐は code で行う**(message でしない)。
//   - 新しい失敗パターンは、まずここに code を足してから実装する(後付けの無秩序な文字列を作らない)。
//
// Result を作る箇所はここの定数だけを使う。Catalog がこのパッケージの単一真実で、
// 追加・削除は codes_test.go の golden がレビューに載せる。

// 警告コード(warnings・処理は続行)。
const (
	WarnWinUnsigned       = "win_unsigned"        // Windows 成果物が未署名(証明書なし)
	WarnDarwinUnnotarized = "darwin_unnotarized"  // darwin 署名済みだが未公証
	WarnChannelSkipped    = "channel_skipped"     // チャネルを skip(トークン/設定不足)
	WarnDriftDetected     = "drift_detected"      // 記録と実体が食い違い(status の drift / verify の陳腐化した記録)
	WarnGatedPending      = "gated_pending"       // gated チャネルが審査待ち
	WarnGoinstallOnlyGo   = "goinstall_only_go"   // goinstall 指定だが Go ターゲットでない
	WarnTapWillBeCreated  = "tap_will_be_created" // 自前 tap/bucket が未作成で作る予定
	WarnInitMissing       = "init_missing"        // AGENTS.md / CLAUDE.md が wharfy を指していない(wharfy init 未実施)
	// WarnDeprecateNoSurface: 畳むと宣言したチャネルに告知を載せる欄が無い(goinstall/container 等)。
	// 黙って落とすと配布者は「告知したつもり」で気づけないので、載らなかったことを明示する(D-3)。
	WarnDeprecateNoSurface = "deprecate_no_notice_surface"
	// WarnDeprecateOrphan: channels に無いチャネルへの deprecate 宣言。告知の更新が止まっている(D-3)。
	WarnDeprecateOrphan = "deprecate_orphan"
	// WarnDeprecateFrozen: ship:false のチャネルを最後に配った版で据え置いた(D-3)。凍結は
	// 「新版が出ない」という不在の形で現れるので、何が据え置かれたのかを publish のたびに言う。
	WarnDeprecateFrozen = "deprecate_frozen"
	// WarnStaleGenerator: 実行中の wharfy が、これからリリースする repo の HEAD からビルドされていない。
	// 生成物(install.sh / formula / cask …)は実行中のバイナリが作るので、HEAD で直した生成器が
	// 成果物に入らない。wharfy 自身をリリースするときだけ起きうる(module path が一致する repo)。
	WarnStaleGenerator = "stale_generator"
	// WarnPkgNotIndexed: hosted repo(apt/rpm)へのアップロードは成功したのに、その版が公開
	// リポジトリの索引にまだ出ていない。アップロードが 200 を返す以上 publish は成功と言うが、
	// 利用者はまだ誰も入れられない —— この差は配布者からは見えない。理由は 2 つあり、どちらも初回に踏む:
	// 索引の生成待ち(数分)か、パッケージが非公開のまま(fury は既定で非公開として受け取り、
	// ダッシュボードで公開に切り替えるまで公開 repo に載せない)。
	WarnPkgNotIndexed = "pkg_not_indexed"
)

// エラーコード(errors・ok=false で停止)。
const (
	ErrConfigInvalid      = "config_invalid"       // wharfy.yaml が不正(スキーマ違反)
	ErrMainAmbiguous      = "main_ambiguous"       // main を推測できない(複数 main)
	ErrGithubUnresolved   = "github_unresolved"    // github を推測できない(remote 不在等)
	ErrTagMissing         = "tag_missing"          // tag 上でない/tag が無い
	ErrBuildFailed        = "build_failed"         // クロスビルド失敗
	ErrBuilderUnavailable = "builder_unavailable"  // 下層ビルダ(GoReleaser)が見つからない/起動不可
	ErrSignFailed         = "sign_failed"          // 署名失敗(codesign 不在/失敗・依頼①)
	ErrTokenMissing       = "token_missing"        // その操作に必須のトークン未設定
	ErrAuthFailed         = "auth_failed"          // トークン/鍵はあるが認証失敗
	ErrKeychainFailed     = "keychain_failed"      // OS keychain への保存/読み出しに失敗(ロック/権限)
	ErrTargetCreateFailed = "target_create_failed" // 自前 tap/bucket/repo 作成失敗
	ErrInitWriteFailed    = "init_write_failed"    // init で AGENTS.md / CLAUDE.md の読み書きに失敗(権限等)
	ErrConsentRequired    = "consent_required"     // strict gated への申請に明示同意が必要(未同意)
	ErrPublishFailed      = "publish_failed"       // チャネルへの発行失敗
	// ErrChannelNotConfigured: wharfy.yaml の channels: に無いチャネルを名指しで publish / verify しようとした。
	// 畳んだチャネルへの発行は、配布者が設定へ書き戻したときにだけ起こる(配布は明示ゲート)。
	ErrChannelNotConfigured = "channel_not_configured"
	ErrChecksumMismatch     = "checksum_mismatch" // manifest の sha256 が実アセットと不一致(#10 自己検査)
	ErrProbeFailed          = "probe_failed"      // 実体照合に失敗
	ErrNetworkError         = "network_error"     // 一時的なネットワーク失敗
	ErrVerifyFailed         = "verify_failed"     // verify で install/実行が失敗
	// ErrNothingToVerify: channels: のどのチャネルも検証できなかった(D-4)。「何も確かめられなかった」を
	// 緑で返すと CI が壊れた配布を通すので、検証成功と区別して ok=false にする。
	ErrNothingToVerify = "nothing_to_verify"
	// ErrStaleGeneratorBlocked: 版ズレ(WarnStaleGenerator と同じ事象)のまま apply(--yes)しようとした。
	// 警告どまりで済むのは plan 経路だけで、--yes は非対話ゆえ警告が読まれる頃にはもう上がっている。
	// 判断できる時点で止めるため、apply では警告ではなく拒否にする(--allow-stale-generator で上書き可)。
	ErrStaleGeneratorBlocked = "stale_generator_blocked"
	ErrInternal              = "internal" // 想定外(バグ)
)

// CodeKind は正準カタログ内での分類。warning=処理続行 / error=ok=false で停止。
type CodeKind string

const (
	KindWarning CodeKind = "warning"
	KindError   CodeKind = "error"
)

// CatalogEntry は正準カタログの 1 行。Summary は「いつ起きるか」。
type CatalogEntry struct {
	Code    string
	Kind    CodeKind
	Summary string
}

// Catalog は 09 の正準リストをコードに写したもの。コード定数と 1:1 で対応する。
// 並びは 09 の表と同順。
var Catalog = []CatalogEntry{
	{WarnWinUnsigned, KindWarning, "Windows 成果物が未署名(証明書なし)"},
	{WarnDarwinUnnotarized, KindWarning, "darwin 署名済みだが未公証"},
	{WarnChannelSkipped, KindWarning, "チャネルを skip(トークン/設定不足)"},
	{WarnDriftDetected, KindWarning, "記録と実体が食い違い(status の drift / verify の陳腐化した記録)"},
	{WarnGatedPending, KindWarning, "gated チャネルが審査待ち"},
	{WarnGoinstallOnlyGo, KindWarning, "goinstall 指定だが Go ターゲットでない"},
	{WarnTapWillBeCreated, KindWarning, "自前 tap/bucket が未作成で作る予定"},
	{WarnInitMissing, KindWarning, "AGENTS.md / CLAUDE.md が wharfy を指していない(wharfy init 未実施)"},
	{WarnDeprecateNoSurface, KindWarning, "畳むチャネルに告知を載せる欄が無い(latest.json 経由でのみ届く)"},
	{WarnDeprecateOrphan, KindWarning, "channels に無いチャネルへの deprecate 宣言(告知の更新が止まっている)"},
	{WarnDeprecateFrozen, KindWarning, "ship:false のチャネルを最後に配った版で据え置いた"},
	{WarnStaleGenerator, KindWarning, "実行中の wharfy が repo の HEAD からビルドされていない(生成物が古い生成器で作られる)"},
	{WarnPkgNotIndexed, KindWarning, "hosted repo へ上げた版が公開索引にまだ無い(取り込み待ち、または非公開のまま)"},

	{ErrConfigInvalid, KindError, "wharfy.yaml が不正(スキーマ違反)"},
	{ErrMainAmbiguous, KindError, "main を推測できない(複数 main)"},
	{ErrGithubUnresolved, KindError, "github を推測できない(remote 不在等)"},
	{ErrTagMissing, KindError, "tag 上でない/tag が無い"},
	{ErrBuildFailed, KindError, "クロスビルド失敗"},
	{ErrBuilderUnavailable, KindError, "下層ビルダが見つからない/起動不可"},
	{ErrSignFailed, KindError, "署名失敗(codesign 不在/失敗)"},
	{ErrTokenMissing, KindError, "その操作に必須のトークン未設定"},
	{ErrAuthFailed, KindError, "トークン/鍵はあるが認証失敗"},
	{ErrKeychainFailed, KindError, "OS keychain への保存/読み出しに失敗(ロック/権限)"},
	{ErrTargetCreateFailed, KindError, "自前 tap/bucket/repo 作成失敗"},
	{ErrInitWriteFailed, KindError, "init で AGENTS.md / CLAUDE.md の読み書きに失敗(権限等)"},
	{ErrConsentRequired, KindError, "strict gated への申請に明示同意が必要(未同意)"},
	{ErrPublishFailed, KindError, "チャネルへの発行失敗"},
	{ErrChannelNotConfigured, KindError, "channels: に無いチャネルを名指しで publish / verify しようとした"},
	{ErrChecksumMismatch, KindError, "manifest の sha256 が実アセットと不一致(自己検査)"},
	{ErrProbeFailed, KindError, "実体照合に失敗"},
	{ErrNetworkError, KindError, "一時的なネットワーク失敗"},
	{ErrVerifyFailed, KindError, "verify で install/実行が失敗"},
	{ErrNothingToVerify, KindError, "channels: のどのチャネルも検証できなかった(検証成功と区別する)"},
	{ErrStaleGeneratorBlocked, KindError, "版ズレのまま --yes で apply しようとした(--allow-stale-generator で上書き可)"},
	{ErrInternal, KindError, "想定外(バグ)"},
}

// KnownCode は code が正準カタログに存在するかを返す。Result 組み立て時の自己点検に使う。
func KnownCode(code string) bool {
	for _, e := range Catalog {
		if e.Code == code {
			return true
		}
	}
	return false
}

// catalogCodes は Catalog の code 集合(テスト・補完生成などの照合用)。
func catalogCodes() map[string]CodeKind {
	m := make(map[string]CodeKind, len(Catalog))
	for _, e := range Catalog {
		m[e.Code] = e.Kind
	}
	return m
}
