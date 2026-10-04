package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestVerify(t *testing.T) {
	body := []byte(`{"action":"opened"}`)
	mac := hmac.New(sha256.New, []byte("secret"))
	_, _ = mac.Write(body)
	signature := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if !Verify(body, signature, "secret") {
		t.Fatal("valid signature rejected")
	}
	if Verify([]byte("tampered"), signature, "secret") {
		t.Fatal("tampered payload accepted")
	}
	if Verify(body, signature, "") {
		t.Fatal("empty secret accepted")
	}
}
