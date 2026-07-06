package channel

import (
	"context"
	"fmt"
)

// cask_publisher.go — Cask Publisher(owned・即時)。自前 tap の Casks/<token>.rb を所有する(03)。
// homebrew.go(Formula)の対で、同じ tap・同じ TapStore に書く。Formula と Cask が 1 つの tap に
// 同居するので、status は両方を同じ probe 経路で照合できる(状態一元化・依頼④)。

// Cask は cask チャネルの Publisher。Tap は "owner/homebrew-<project>"(Formula と共有可)。
type Cask struct {
	Token string // cask 識別子/ファイル名(例: <project>-app)
	Tap   string // owner/homebrew-<project>
	Input CaskInput
	Store TapStore
}

func (c *Cask) Name() string { return "cask" }
func (c *Cask) Kind() string { return KindOwned }

// CaskPath は tap 内の cask の場所(所有対象＝この path だけを書く)。
func (c *Cask) CaskPath() string {
	return "Casks/" + c.Token + ".rb"
}

// ownedArtifact は publish.json の owned_artifact。
func (c *Cask) ownedArtifact() string {
	return c.Tap + ":" + c.CaskPath()
}

// Plan は cask を生成し、tap 上の現状と突き合わせて操作と差分を返す(書かない)。
func (c *Cask) Plan(ctx context.Context) (PlanItem, error) {
	want := GenerateCask(c.Input)
	base, found, err := c.Store.Get(ctx, c.CaskPath())
	if err != nil {
		return PlanItem{}, fmt.Errorf("probe tap cask: %w", err)
	}
	item := PlanItem{
		Channel:       c.Name(),
		Kind:          c.Kind(),
		OwnedArtifact: c.ownedArtifact(),
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

// Publish は差分があれば tap に書く。noop なら書かない。書くのは owned cask のみ(03)。
func (c *Cask) Publish(ctx context.Context) (PlanItem, PubResult, error) {
	item, err := c.Plan(ctx)
	if err != nil {
		return PlanItem{}, PubResult{}, err
	}
	if item.Action == ActionNoop {
		return item, PubResult{}, nil
	}
	want := GenerateCask(c.Input)
	msg := fmt.Sprintf("wharfy: %s %s %s", item.Action, c.Token, c.Input.Version)
	commit, err := c.Store.Put(ctx, c.CaskPath(), want, msg)
	if err != nil {
		return item, PubResult{}, err
	}
	return item, PubResult{Commit: commit}, nil
}

// RepoExists は tap リポジトリが在るか(dry-run の tap_will_be_created 予告に使う)。
func (c *Cask) RepoExists(ctx context.Context) (bool, error) { return c.Store.Exists(ctx) }

// EnsureRepo は tap が無ければ作る(--yes の上でのみ呼ばれる・ADR-8)。created=作成したか。
func (c *Cask) EnsureRepo(ctx context.Context) (bool, error) { return ensureRepo(ctx, c.Store) }

// Probe は tap 上の cask の版を返す(実体・04 の照合の基点)。
func (c *Cask) Probe(ctx context.Context) (RemoteState, error) {
	base, found, err := c.Store.Get(ctx, c.CaskPath())
	if err != nil {
		return RemoteState{}, err
	}
	if !found {
		return RemoteState{Found: false}, nil
	}
	return RemoteState{Version: CaskVersion(base), Found: true}, nil
}
