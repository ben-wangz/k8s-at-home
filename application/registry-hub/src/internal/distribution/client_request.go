package distribution

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
)

func (c *Client) request(ctx context.Context, method, registryPath string, query url.Values, headers http.Header, body io.Reader, contentLength int64) (*http.Request, error) {
	if !strings.HasPrefix(registryPath, "/v2/") && registryPath != "/v2/" {
		return nil, errors.New("internal Registry path must begin with /v2/")
	}
	decodedPath, err := url.PathUnescape(registryPath)
	if err != nil || strings.ContainsAny(decodedPath, "\r\n") {
		return nil, errors.New("internal Registry path is invalid")
	}
	target := *c.base
	basePath := strings.TrimSuffix(c.base.Path, "/")
	target.Path = path.Join(basePath, decodedPath)
	if strings.HasSuffix(registryPath, "/") && !strings.HasSuffix(target.Path, "/") {
		target.Path += "/"
	}
	target.RawPath = ""
	if query != nil {
		target.RawQuery = query.Encode()
	} else {
		target.RawQuery = ""
	}
	request, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, err
	}
	request.Host = c.base.Host
	if contentLength == 0 {
		request.Body = http.NoBody
	}
	request.ContentLength = contentLength
	for name, values := range headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	} else if c.user != "" {
		request.SetBasicAuth(c.user, c.password)
	}
	return request, nil
}

func sameOriginBearerChallenge(challenges []string, base *url.URL) bool {
	for _, challenge := range challenges {
		lower := strings.ToLower(challenge)
		if !strings.Contains(lower, "bearer") {
			continue
		}
		start := strings.Index(lower, "realm=")
		if start < 0 {
			continue
		}
		value := strings.TrimSpace(challenge[start+len("realm="):])
		if len(value) < 2 || value[0] != '"' {
			continue
		}
		end := strings.IndexByte(value[1:], '"')
		if end < 0 {
			continue
		}
		realm, err := url.Parse(value[1 : end+1])
		if err == nil && strings.EqualFold(realm.Scheme, base.Scheme) && strings.EqualFold(realm.Host, base.Host) {
			return true
		}
	}
	return false
}
