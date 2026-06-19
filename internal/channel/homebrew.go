package channel

import (
	"context"
	"fmt"
)

// homebrew.go — Homebrew Publisher(owned・即時)。自前 tap の Formula/<project>.rb を所有する(03)。
// Plan で差分を見せ、Publish で tap に書く。Probe で tap 上の版を確認し status の照合に使う(04)。

// Homebrew は homebrew チャネルの Publisher。Tap は "owner/homebrew-<project>"。
type Homebrew struct {
	Project string
	Tap     string // owner/homebrew-<project>
	Input   FormulaInput
	Store   TapStore
}

func (h *Homebrew) Name() string { return "homebrew" }
func (h *Homebrew) Kind() string { return KindOwned }

// FormulaPath は tap 内の formula の場所(所有対象＝この path だけを書く)。
func (h *Homebrew) FormulaPath() string {
	return "Formula/" + h.Project + ".rb"
}

// ownedArtifact は publish.json の owned_artifact(利用者のものは構造上ここに入らない)。
func (h *Homebrew) ownedArtifact() string {
	return h.Tap + ":" + h.FormulaPath()
}

// Plan は formula を生成し、tap 上の現状と突き合わせて操作と差分を返す(書かない)。
func (h *Homebrew) Plan(ctx context.Context) (PlanItem, error) {
	want := GenerateFormula(h.Input)
	base, found, err := h.Store.Get(ctx, h.FormulaPath())
	if err != nil {
		return PlanItem{}, fmt.Errorf("probe tap formula: %w", err)
	}
	item := PlanItem{
		Channel:       h.Name(),
		Kind:          h.Kind(),
		OwnedArtifact: h.ownedArtifact(),
	}
	switch {
	case !found:
		item.Action = ActionCreate
		item.Diff = Diff("", want)
	case base == want:
		item.Action = ActionNoop
	default:
		item.Action = ActionUpdate
		item.Diff = Diff(base, want)
	}
	return item, nil
}

// Publish は差分があれば tap に書く。noop なら書かない。書くのは owned formula のみ(03)。
func (h *Homebrew) Publish(ctx context.Context) (PlanItem, PubResult, error) {
	item, err := h.Plan(ctx)
	if err != nil {
		return PlanItem{}, PubResult{}, err
	}
	if item.Action == ActionNoop {
		return item, PubResult{}, nil
	}
	want := GenerateFormula(h.Input)
	msg := fmt.Sprintf("wharfy: %s %s %s", item.Action, h.Project, h.Input.Version)
	commit, err := h.Store.Put(ctx, h.FormulaPath(), want, msg)
	if err != nil {
		return item, PubResult{}, err
	}
	return item, PubResult{Commit: commit}, nil
}

// RepoExists は自前 tap リポジトリが在るか(dry-run の tap_will_be_created 予告に使う)。
func (h *Homebrew) RepoExists(ctx context.Context) (bool, error) { return h.Store.Exists(ctx) }

// EnsureRepo は tap が無ければ作る(--yes の上でのみ呼ばれる・ADR-8)。created=作成したか。
func (h *Homebrew) EnsureRepo(ctx context.Context) (bool, error) { return ensureRepo(ctx, h.Store) }

// Probe は tap 上の formula の版を返す(実体・04 の照合の基点)。
func (h *Homebrew) Probe(ctx context.Context) (RemoteState, error) {
	base, found, err := h.Store.Get(ctx, h.FormulaPath())
	if err != nil {
		return RemoteState{}, err
	}
	if !found {
		return RemoteState{Found: false}, nil
	}
	return RemoteState{Version: FormulaVersion(base), Found: true}, nil
}
