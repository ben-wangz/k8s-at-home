package frpcctl

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/url"
	"testing"
	"time"
)

func TestDingTalkSignedURL(t *testing.T) {
	t.Parallel()
	credentials := WebhookCredentials{
		Type:   "dingtalk",
		URL:    "https://oapi.dingtalk.com/robot/send?access_token=test-token",
		Secret: "SECtest-secret",
	}
	now := time.UnixMilli(1_700_000_000_123)
	signed, err := dingTalkSignedURL(credentials, now)
	if err != nil {
		t.Fatalf("sign URL: %v", err)
	}
	parsed, err := url.Parse(signed)
	if err != nil {
		t.Fatalf("parse signed URL: %v", err)
	}
	if parsed.Query().Get("timestamp") != "1700000000123" {
		t.Errorf("unexpected timestamp %q", parsed.Query().Get("timestamp"))
	}
	mac := hmac.New(sha256.New, []byte(credentials.Secret))
	_, _ = mac.Write([]byte("1700000000123\n" + credentials.Secret))
	expectedSignature := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if parsed.Query().Get("sign") != expectedSignature {
		t.Errorf("unexpected signature %q", parsed.Query().Get("sign"))
	}
}
