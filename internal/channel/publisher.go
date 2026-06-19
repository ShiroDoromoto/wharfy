// Package channel はチャネル発行層(設計 01 Publisher / 03 非破壊境界 / 11)。
//
// Plan(差分プレビュー)と Publish(実書き込み)を分け、「差分を見せてから書く」を全チャネル
// 共通で実現する(02・03)。所有する配布物だけが書き込み対象＝利用者のソース・CI は触らない。
// スライス1 は homebrew(owned)1 本。型をここで固めてから横展開する(08 §5)。
package channel

import "context"

// Kind はチャネル種別。owned=即時発行 / gated=申請を組み立て状態追跡(11)。
const (
	KindOwned = "owned"
	KindGated = "gated"
)

// Action は plan の操作(schemas/publish.json planItem.action)。
const (
	ActionCreate  = "create"  // 配布物が無いので新規作成
	ActionUpdate  = "update"  // 既存と差分あり
	ActionNoop    = "noop"    // 既存と同一(書くことなし)
	ActionSkip    = "skip"    // 設定/トークン不足などで見送り
	ActionPrepare = "prepare" // gated: 申請のみ組み立て
)

// PlanItem は 1 チャネルへの所有配布物に対する操作(publish.json planItem と同形)。
type PlanItem struct {
	Channel       string `json:"channel"`
	Kind          string `json:"kind,omitempty"`
	OwnedArtifact string `json:"owned_artifact,omitempty"`
	Action        string `json:"action"`
	Diff          string `json:"diff,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

// PubResult は実書き込み(Publish)の結果。
type PubResult struct {
	Commit string
	URL    string
}

// RemoteState は実体照合(Probe)の結果。status の drift 判定に使う(04)。
type RemoteState struct {
	Version string
	Found   bool
}

// Publisher はチャネル発行の境界(01)。Plan は書かない・Publish が書く。
type Publisher interface {
	Name() string
	Kind() string
	Plan(ctx context.Context) (PlanItem, error)
	Publish(ctx context.Context) (PlanItem, PubResult, error)
	Probe(ctx context.Context) (RemoteState, error)
}

// TapStore は自前 tap(owned リポジトリ)への読み書き境界。
// 実体は GitHub。テストは InMemoryTapStore で差し替える(末端は差し替え可能・01)。
type TapStore interface {
	// Get は path の現在の内容を返す。無ければ found=false(エラーではない)。
	Get(ctx context.Context, path string) (content string, found bool, err error)
	// Put は path に content を書き(commit message 付き)、commit を返す。書き込みは owned のみ(03)。
	Put(ctx context.Context, path, content, message string) (commit string, err error)
	// Exists は所有リポジトリ(tap/bucket)自体が在るか。無ければ false。
	Exists(ctx context.Context) (bool, error)
	// Create は所有リポジトリを作る(初期化付き)。利用者の明示同意(--yes)の上でのみ呼ぶ(03/ADR-8)。
	Create(ctx context.Context) error
}

// RepoBacked は自前リポジトリ(tap/bucket)に書く owned チャネル(homebrew/scoop)。
// 未作成なら --yes で wharfy が作成する(ADR-8)。dry-run は RepoExists で予告に使う。
type RepoBacked interface {
	RepoExists(ctx context.Context) (bool, error)
	EnsureRepo(ctx context.Context) (created bool, err error)
}

// ensureRepo は store のリポジトリが無ければ作る(homebrew/scoop 共通)。created=作成したか。
func ensureRepo(ctx context.Context, store TapStore) (bool, error) {
	exists, err := store.Exists(ctx)
	if err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	if err := store.Create(ctx); err != nil {
		return false, err
	}
	return true, nil
}
