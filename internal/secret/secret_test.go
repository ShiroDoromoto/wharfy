package secret

import (
	"errors"
	"testing"

	"github.com/zalando/go-keyring"
)

func TestSecretRoundTrip(t *testing.T) {
	keyring.MockInit() // in-memory keyring(実 OS keychain を触らない)

	if _, err := Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing key → ErrNotFound, got %v", err)
	}
	if err := Set("k", "v"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if v, err := Get("k"); err != nil || v != "v" {
		t.Errorf("Get = (%q,%v), want (\"v\",nil)", v, err)
	}
	if err := Delete("k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := Get("k"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete → ErrNotFound, got %v", err)
	}
	if err := Delete("k"); err != nil { // 二重削除は no-op(エラーにしない)
		t.Errorf("double delete should be nil, got %v", err)
	}
}
