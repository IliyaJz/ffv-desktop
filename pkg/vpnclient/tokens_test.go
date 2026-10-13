package vpnclient

import (
	"testing"
	"time"
)

func TestSavedTokensValid(t *testing.T) {
	// 1. Empty token should be invalid
	empty := &SavedTokens{}
	if empty.AccessTokenValid() {
		t.Error("expected empty token to be invalid")
	}

	// 2. Fresh token should be valid
	fresh := &SavedTokens{
		AccessToken: "test-token",
		ObtainedAt:  time.Now(),
		ExpiresIn:   3600,
	}
	if !fresh.AccessTokenValid() {
		t.Error("expected fresh token to be valid")
	}

	// 3. Expired token should be invalid
	expired := &SavedTokens{
		AccessToken: "test-token",
		ObtainedAt:  time.Now().Add(-2 * time.Hour),
		ExpiresIn:   3600,
	}
	if expired.AccessTokenValid() {
		t.Error("expected expired token to be invalid")
	}
}
