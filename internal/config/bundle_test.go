package config

import (
	"reflect"
	"testing"
)

func guiBundles() []Bundle {
	return []Bundle{
		{OS: "darwin", Arch: "arm64", Kind: "dmg", Path: "dist/App-1.0.0-arm64.dmg"},
		{OS: "darwin", Arch: "amd64", Kind: "dmg", Path: "dist/App-1.0.0-amd64.dmg"},
	}
}

// TestResolveBundleSkipsMain: BYO-bundle でも(prebuilt と同じく)main を解決しない — 非 Go/GUI で
// go list が失敗しても Resolve はエラーにならず、cfg.Bundle=true になる(依頼①)。
func TestResolveBundleSkipsMain(t *testing.T) {
	r := prebuiltResolver("https://github.com/acme/app.git", "")
	cfg, err := r.Resolve(File{Bundle: &BundleInput{Bundles: guiBundles()}})
	if err != nil {
		t.Fatalf("bundle Resolve should not error on missing main: %v", err)
	}
	if cfg.Main != "" {
		t.Errorf("main = %q, want empty in bundle mode", cfg.Main)
	}
	if !cfg.Bundle {
		t.Error("cfg.Bundle = false, want true")
	}
}

// TestResolveBundleDefaultChannels: channels 未指定なら GUI 既定列 [cask, releases](依頼書 §6)。
func TestResolveBundleDefaultChannels(t *testing.T) {
	r := prebuiltResolver("https://github.com/acme/app.git", "")
	cfg, err := r.Resolve(File{Bundle: &BundleInput{Bundles: guiBundles()}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := channelNames(cfg); !reflect.DeepEqual(got, DefaultBundleChannels) {
		t.Errorf("channels = %v, want %v", got, DefaultBundleChannels)
	}
}

// TestResolveCaskTapSharesFormula: cask の発行先 tap は既定で Formula と同じ <owner>/homebrew-<project>
// (同居=状態一元化・依頼④)。cask.tap で上書きもできる。
func TestResolveCaskTapSharesFormula(t *testing.T) {
	r := prebuiltResolver("https://github.com/acme/app.git", "")
	cfg, err := r.Resolve(File{Bundle: &BundleInput{Bundles: guiBundles()}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := targetOf(cfg, "cask"); got != "acme/homebrew-app" {
		t.Errorf("cask tap = %q, want acme/homebrew-app (shared with formula)", got)
	}

	// cask.tap の明示上書き。
	cfg2, _ := r.Resolve(File{
		Bundle: &BundleInput{Bundles: guiBundles()},
		Cask:   &CaskInput{Tap: "acme/homebrew-tools"},
	})
	if got := targetOf(cfg2, "cask"); got != "acme/homebrew-tools" {
		t.Errorf("cask tap override = %q, want acme/homebrew-tools", got)
	}
}

// TestResolveCaskAlongsideFormula: 1 つの config が homebrew と cask を同時に宣言でき、両者が同じ
// tap を指す(1 つの status に Formula と Cask を並べる土台・依頼④)。
func TestResolveCaskAlongsideFormula(t *testing.T) {
	r := prebuiltResolver("https://github.com/acme/app.git", "")
	in := File{
		Bundle:   &BundleInput{Bundles: guiBundles()},
		Channels: []string{"homebrew", "cask", "releases"},
	}
	cfg, err := r.Resolve(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := channelNames(cfg); !reflect.DeepEqual(got, []string{"homebrew", "cask", "releases"}) {
		t.Errorf("channels = %v", got)
	}
	if targetOf(cfg, "homebrew") != targetOf(cfg, "cask") {
		t.Errorf("formula tap %q and cask tap %q should be the same repo",
			targetOf(cfg, "homebrew"), targetOf(cfg, "cask"))
	}
}

// TestIsBundle: bundle 宣言の有無を正しく判定する(空宣言は false)。
func TestIsBundle(t *testing.T) {
	if IsBundle(File{}) {
		t.Error("empty File should not be bundle mode")
	}
	if IsBundle(File{Bundle: &BundleInput{}}) {
		t.Error("bundle with no bundles should not be bundle mode")
	}
	if !IsBundle(File{Bundle: &BundleInput{Bundles: guiBundles()}}) {
		t.Error("bundle with bundles should be bundle mode")
	}
}
