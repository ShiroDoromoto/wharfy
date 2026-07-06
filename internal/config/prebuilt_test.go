package config

import (
	"fmt"
	"reflect"
	"testing"
)

// prebuiltResolver は非 Go リポを模す: MainPkgs(go list 相当)は必ず失敗する。
// prebuilt モードでは resolveMain を呼ばないので、この失敗が漏れないことを検証できる。
func prebuiltResolver(origin, module string) *Resolver {
	return &Resolver{
		Root:       "/fake/root",
		OriginURL:  func(string) (string, error) { return origin, nil },
		MainPkgs:   func(string) ([]string, error) { return nil, fmt.Errorf("go list ./...: not a go module") },
		ModulePath: func(string) (string, error) { return module, nil },
	}
}

func rustBinaries() []PrebuiltBinary {
	return []PrebuiltBinary{
		{OS: "darwin", Arch: "arm64", Path: "dist/app-darwin-arm64"},
		{OS: "darwin", Arch: "amd64", Path: "dist/app-darwin-amd64"},
		{OS: "linux", Arch: "amd64", Path: "dist/app-linux-amd64"},
		{OS: "linux", Arch: "arm64", Path: "dist/app-linux-arm64"},
		{OS: "windows", Arch: "amd64", Path: "dist/app-windows-amd64.exe"},
	}
}

// TestResolvePrebuiltSkipsMain: BYO-binary では main を解決せず(go list を叩かず)、
// MainPkgs が失敗しても Resolve はエラーを返さない — main は空(依頼①・付録A の症状解消)。
func TestResolvePrebuiltSkipsMain(t *testing.T) {
	r := prebuiltResolver("https://github.com/acme/app.git", "")
	cfg, err := r.Resolve(File{Prebuilt: &PrebuiltInput{Binaries: rustBinaries()}})
	if err != nil {
		t.Fatalf("prebuilt Resolve should not error on missing main: %v", err)
	}
	if cfg.Main != "" {
		t.Errorf("main = %q, want empty in prebuilt mode", cfg.Main)
	}
	if !cfg.Prebuilt {
		t.Error("cfg.Prebuilt = false, want true")
	}
}

// TestResolvePrebuiltBuildMatrix: build 行列は持ち込みバイナリの (os,arch) を宣言順・重複排除で導く。
func TestResolvePrebuiltBuildMatrix(t *testing.T) {
	r := prebuiltResolver("https://github.com/acme/app.git", "")
	cfg, err := r.Resolve(File{Prebuilt: &PrebuiltInput{Binaries: rustBinaries()}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantOS := []string{"darwin", "linux", "windows"}
	wantArch := []string{"arm64", "amd64"}
	if cfg.Build == nil {
		t.Fatal("build is nil")
	}
	if !reflect.DeepEqual(cfg.Build.GOOS, wantOS) {
		t.Errorf("goos = %v, want %v", cfg.Build.GOOS, wantOS)
	}
	if !reflect.DeepEqual(cfg.Build.GOARCH, wantArch) {
		t.Errorf("goarch = %v, want %v", cfg.Build.GOARCH, wantArch)
	}
}

// TestResolvePrebuiltDefaultChannels: channels 未指定なら goinstall を含まない既定列(依頼②)。
func TestResolvePrebuiltDefaultChannels(t *testing.T) {
	r := prebuiltResolver("https://github.com/acme/app.git", "")
	cfg, err := r.Resolve(File{Prebuilt: &PrebuiltInput{Binaries: rustBinaries()}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := channelNames(cfg); !reflect.DeepEqual(got, DefaultPrebuiltChannels) {
		t.Errorf("channels = %v, want %v", got, DefaultPrebuiltChannels)
	}
	if targetOf(cfg, "goinstall") != "" {
		t.Error("goinstall should not appear in prebuilt mode")
	}
}

// TestResolvePrebuiltDropsGoOnlyChannels: goinstall/homebrew-core を明示しても非 Go では外れる(依頼②)。
func TestResolvePrebuiltDropsGoOnlyChannels(t *testing.T) {
	r := prebuiltResolver("https://github.com/acme/app.git", "")
	in := File{
		Prebuilt: &PrebuiltInput{Binaries: rustBinaries()},
		Channels: []string{"releases", "homebrew", "goinstall", "homebrew-core"},
	}
	cfg, err := r.Resolve(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"releases", "homebrew"}
	if got := channelNames(cfg); !reflect.DeepEqual(got, want) {
		t.Errorf("channels = %v, want %v (go-only dropped)", got, want)
	}
}

// TestNonPrebuiltUnchanged: prebuilt 無しの通常パスは従来どおり(main 解決・既定列・goinstall あり)。
func TestNonPrebuiltUnchanged(t *testing.T) {
	r := stubResolver("https://github.com/acme/mytool.git", []string{"./cmd/mytool"}, "github.com/acme/mytool")
	cfg, err := r.Resolve(File{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Prebuilt {
		t.Error("cfg.Prebuilt = true, want false for a normal Go repo")
	}
	if cfg.Main != "./cmd/mytool" {
		t.Errorf("main = %q, want ./cmd/mytool", cfg.Main)
	}
	if got := channelNames(cfg); !reflect.DeepEqual(got, DefaultChannels) {
		t.Errorf("channels = %v, want %v", got, DefaultChannels)
	}
}

func TestIsPrebuiltAndCompatible(t *testing.T) {
	if IsPrebuilt(File{}) {
		t.Error("empty File should not be prebuilt")
	}
	if IsPrebuilt(File{Prebuilt: &PrebuiltInput{}}) {
		t.Error("prebuilt with no binaries should not count as prebuilt")
	}
	if !IsPrebuilt(File{Prebuilt: &PrebuiltInput{Binaries: rustBinaries()}}) {
		t.Error("prebuilt with binaries should count as prebuilt")
	}
	for _, name := range []string{"goinstall", "homebrew-core"} {
		if PrebuiltCompatible(name) {
			t.Errorf("%s should be incompatible with prebuilt", name)
		}
	}
	for _, name := range []string{"homebrew", "scoop", "releases", "script", "apt", "rpm", "container", "aur"} {
		if !PrebuiltCompatible(name) {
			t.Errorf("%s should be compatible with prebuilt", name)
		}
	}
}
