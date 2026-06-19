package channel

import (
	"context"
	"strings"
	"testing"
)

func newHomebrew(store TapStore) *Homebrew {
	return &Homebrew{
		Project: "demo",
		Tap:     "acme/homebrew-demo",
		Store:   store,
		Input:   sampleInput(),
	}
}

func TestHomebrewPlanCreate(t *testing.T) {
	hb := newHomebrew(NewInMemoryTapStore()) // tap 空 → create
	item, err := hb.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if item.Action != ActionCreate {
		t.Errorf("action = %q, want create", item.Action)
	}
	if item.OwnedArtifact != "acme/homebrew-demo:Formula/demo.rb" {
		t.Errorf("owned_artifact = %q", item.OwnedArtifact)
	}
	if item.Kind != KindOwned || !strings.Contains(item.Diff, "+class Demo") {
		t.Errorf("create plan wrong: %+v", item)
	}
}

func TestHomebrewPlanNoopAndUpdate(t *testing.T) {
	store := NewInMemoryTapStore()
	hb := newHomebrew(store)
	// 既に同一 formula があれば noop。
	store.Files[hb.FormulaPath()] = GenerateFormula(hb.Input)
	item, _ := hb.Plan(context.Background())
	if item.Action != ActionNoop || item.Diff != "" {
		t.Errorf("want noop with empty diff, got %+v", item)
	}
	// 版が違えば update＋差分。
	old := hb.Input
	old.Version = "1.0.0"
	store.Files[hb.FormulaPath()] = GenerateFormula(old)
	item, _ = hb.Plan(context.Background())
	if item.Action != ActionUpdate {
		t.Fatalf("want update, got %q", item.Action)
	}
	if !strings.Contains(item.Diff, `-  version "1.0.0"`) || !strings.Contains(item.Diff, `+  version "1.2.3"`) {
		t.Errorf("update diff should show version change:\n%s", item.Diff)
	}
}

func TestHomebrewPublishWritesOnlyWhenNeeded(t *testing.T) {
	store := NewInMemoryTapStore()
	hb := newHomebrew(store)

	item, pub, err := hb.Publish(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if item.Action != ActionCreate || store.Commits != 1 || pub.Commit == "" {
		t.Errorf("first publish should create+commit: action=%q commits=%d commit=%q", item.Action, store.Commits, pub.Commit)
	}
	if store.Files[hb.FormulaPath()] != GenerateFormula(hb.Input) {
		t.Error("tap should hold the generated formula after publish")
	}

	// 同一内容を再 publish → noop で書かない(冪等)。
	_, _, _ = hb.Publish(context.Background())
	if store.Commits != 1 {
		t.Errorf("noop publish must not write; commits = %d, want 1", store.Commits)
	}
}

func TestHomebrewProbe(t *testing.T) {
	store := NewInMemoryTapStore()
	hb := newHomebrew(store)

	rs, _ := hb.Probe(context.Background())
	if rs.Found {
		t.Error("empty tap → not found")
	}
	store.Files[hb.FormulaPath()] = GenerateFormula(hb.Input)
	rs, _ = hb.Probe(context.Background())
	if !rs.Found || rs.Version != "1.2.3" {
		t.Errorf("probe = %+v, want found v1.2.3", rs)
	}
}
