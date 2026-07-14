package main

import (
	"context"
	"strings"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/channel"
	"github.com/ShiroDoromoto/wharfy/internal/output"
	"github.com/ShiroDoromoto/wharfy/internal/state"
)

// prereleasedStore は v0.1.0 が prerelease として上がっている状態を作る。
func prereleasedStore(t *testing.T) *channel.InMemoryReleaseStore {
	t.Helper()
	s := channel.NewInMemoryReleaseStore()
	if err := s.Upload(context.Background(), "v0.1.0", "app 0.1.0", []channel.ReleaseAsset{
		{Name: "app_0.1.0_linux_amd64.tar.gz"},
		{Name: "install.sh"},
	}, channel.ReleaseOptions{Prerelease: true}); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestPromotePlan: 既定は plan。昇格の前に消費者の目で確かめろ、と次の一手で言う。
func TestPromotePlan(t *testing.T) {
	root := scratchPrebuilt(t)
	tagScratch(t, root, "v0.1.0")
	chdir(t, root)

	store := prereleasedStore(t)
	defer swapReleaseStore(store)()

	res := runPromote(context.Background(), mustLookup(t, "promote"), nil)
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	if store.Promoted != 0 {
		t.Error("the plan must not touch the release")
	}
	var verify bool
	for _, n := range res.Next {
		if strings.Contains(n.Do, "wharfy verify") {
			verify = true
		}
	}
	if !verify {
		t.Errorf("next should point at verifying it first: %+v", res.Next)
	}
}

// TestPromoteApply: --yes は latest に切り替え、台帳の「上げただけ」の印を落とす。
// 資産は 1 バイトも上げ直さない —— 検証したバイト列がそのまま配られる。
func TestPromoteApply(t *testing.T) {
	root := scratchPrebuilt(t)
	tagScratch(t, root, "v0.1.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")

	store := prereleasedStore(t)
	uploadsBefore := store.Uploads
	defer swapReleaseStore(store)()
	defer func() { flagYes = false }()
	flagYes = true

	// release --prerelease が残した台帳(利用者にはまだ届いていない)。
	st, _ := state.Load(root, "app")
	st.Publish = map[string]state.PublishRecord{
		"releases": {Version: "0.1.0", Target: "acme/app", At: "2026-01-01T00:00:00Z", Prerelease: true},
		"script":   {Version: "0.1.0", Target: "acme/app release:install.sh", At: "2026-01-01T00:00:00Z", Prerelease: true},
	}
	if err := state.Save(root, st); err != nil {
		t.Fatal(err)
	}

	res := runPromote(context.Background(), mustLookup(t, "promote"), nil)
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	if store.Promoted != 1 || store.Pre["v0.1.0"] {
		t.Errorf("v0.1.0 should now be the latest release: promoted=%d pre=%v", store.Promoted, store.Pre["v0.1.0"])
	}
	if store.Uploads != uploadsBefore {
		t.Errorf("promotion must not re-upload anything (uploads %d → %d)", uploadsBefore, store.Uploads)
	}
	data, ok := res.Data.(promoteData)
	if !ok || !data.Promoted || !data.Latest {
		t.Errorf("data should report the switch: %+v", res.Data)
	}
	st, _ = state.Load(root, "app")
	if st.Publish["releases"].Prerelease || st.Publish["script"].Prerelease {
		t.Errorf("the ledger must drop the not-delivered mark once it is latest: %+v", st.Publish)
	}
}

// TestPromoteIdempotent: 既に latest なら何もせずに緑(CI が二度踏んでも壊れない)。
func TestPromoteIdempotent(t *testing.T) {
	root := scratchPrebuilt(t)
	tagScratch(t, root, "v0.1.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")

	store := channel.NewInMemoryReleaseStore()
	_ = store.Upload(context.Background(), "v0.1.0", "app 0.1.0", []channel.ReleaseAsset{{Name: "a.tar.gz"}}, channel.ReleaseOptions{})
	defer swapReleaseStore(store)()
	defer func() { flagYes = false }()
	flagYes = true

	res := runPromote(context.Background(), mustLookup(t, "promote"), nil)
	if !res.OK {
		t.Fatalf("promoting an already-latest release must be green: %+v", res)
	}
	if store.Promoted != 0 {
		t.Errorf("nothing should have been changed (promoted=%d)", store.Promoted)
	}
	data, _ := res.Data.(promoteData)
	if data.Promoted || !data.Latest {
		t.Errorf("data should say 'already latest, nothing done': %+v", data)
	}
}

// TestPromoteWithoutRelease: 上げてもいない版は latest にできない(昇格はフラグを立てるだけ)。
func TestPromoteWithoutRelease(t *testing.T) {
	root := scratchPrebuilt(t)
	tagScratch(t, root, "v0.1.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")

	defer swapReleaseStore(channel.NewInMemoryReleaseStore())()
	defer func() { flagYes = false }()
	flagYes = true

	res := runPromote(context.Background(), mustLookup(t, "promote"), nil)
	if res.OK {
		t.Fatalf("expected a refusal: %+v", res)
	}
	if len(res.Errors) == 0 || res.Errors[0].Code != output.ErrNoRelease {
		t.Errorf("want %s: %+v", output.ErrNoRelease, res.Errors)
	}
}
