package frpcctl

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

type dingTalkResponse struct {
	ErrorCode    int    `json:"errcode"`
	ErrorMessage string `json:"errmsg"`
}

func sendDingTalk(credentials WebhookCredentials, message string, timeoutSeconds int) error {
	signedURL, err := dingTalkSignedURL(credentials, time.Now())
	if err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]any{
		"msgtype": "text",
		"text":    map[string]string{"content": message},
	})
	if err != nil {
		return fmt.Errorf("encode DingTalk payload: %w", err)
	}
	request, err := http.NewRequest(http.MethodPost, signedURL, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("create DingTalk request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	client := &http.Client{Timeout: time.Duration(timeoutSeconds) * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("call DingTalk webhook: %w", err)
	}
	defer response.Body.Close()
	responseData, err := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	if err != nil {
		return fmt.Errorf("read DingTalk response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("DingTalk returned HTTP %d", response.StatusCode)
	}
	var result dingTalkResponse
	if err := json.Unmarshal(responseData, &result); err != nil {
		return fmt.Errorf("parse DingTalk response: %w", err)
	}
	if result.ErrorCode != 0 {
		return fmt.Errorf("DingTalk returned errcode=%d: %s", result.ErrorCode, result.ErrorMessage)
	}
	return nil
}

func dingTalkSignedURL(credentials WebhookCredentials, now time.Time) (string, error) {
	if err := validateWebhookCredentials(credentials); err != nil {
		return "", err
	}
	parsed, err := url.Parse(credentials.URL)
	if err != nil {
		return "", fmt.Errorf("parse DingTalk URL: %w", err)
	}
	timestamp := strconv.FormatInt(now.UnixMilli(), 10)
	stringToSign := timestamp + "\n" + credentials.Secret
	mac := hmac.New(sha256.New, []byte(credentials.Secret))
	if _, err := mac.Write([]byte(stringToSign)); err != nil {
		return "", fmt.Errorf("sign DingTalk request: %w", err)
	}
	signature := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	query := parsed.Query()
	query.Set("timestamp", timestamp)
	query.Set("sign", signature)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}
