package distribution

import (
	"errors"
	"net/url"
	"strings"

	"registry-hub/internal/route"
)

func (c *Client) ParseLocation(location, internalRepository string) (string, url.Values, error) {
	parsed, err := url.Parse(location)
	if err != nil || parsed.User != nil || (parsed.Host != "" && !parsed.IsAbs()) || parsed.Fragment != "" {
		return "", nil, errors.New("Distribution returned an invalid upload Location")
	}
	if parsed.IsAbs() && (!strings.EqualFold(parsed.Scheme, c.base.Scheme) || !strings.EqualFold(parsed.Host, c.base.Host)) {
		return "", nil, errors.New("Distribution upload Location points outside the internal service")
	}
	cleanPath, err := route.ParseURLPath(parsed.EscapedPath())
	if err != nil {
		return "", nil, errors.New("Distribution upload Location path is invalid")
	}
	base := "/v2/" + internalRepository + "/blobs/uploads"
	if !strings.HasPrefix(cleanPath, base+"/") {
		return "", nil, errors.New("Distribution upload Location repository does not match the request")
	}
	identifier := strings.TrimPrefix(cleanPath, base+"/")
	if identifier == "" || strings.Contains(identifier, "/") {
		return "", nil, errors.New("Distribution upload Location does not contain one upload identifier")
	}
	query := parsed.Query()
	for key, values := range query {
		if key != "_state" || len(values) != 1 {
			return "", nil, errors.New("Distribution upload Location contains an unexpected query")
		}
	}
	return cleanPath, query, nil
}

func (c *Client) RewriteLocation(location, internalRepository, externalRepository string) (string, error) {
	parsed, err := url.Parse(location)
	if err != nil || parsed.User != nil || parsed.Fragment != "" {
		return "", errors.New("Distribution returned an invalid Location")
	}
	if parsed.IsAbs() && (!strings.EqualFold(parsed.Scheme, c.base.Scheme) || !strings.EqualFold(parsed.Host, c.base.Host)) {
		return "", errors.New("Distribution Location points outside the internal service")
	}
	cleanPath, err := route.ParseURLPath(parsed.EscapedPath())
	if err != nil {
		return "", errors.New("Distribution Location path is invalid")
	}
	prefix := "/v2/" + internalRepository + "/"
	if !strings.HasPrefix(cleanPath, prefix) {
		return "", errors.New("Distribution Location repository does not match the request")
	}
	cleanPath = "/v2/" + externalRepository + "/" + strings.TrimPrefix(cleanPath, prefix)
	parsed.Path = cleanPath
	parsed.RawPath = ""
	parsed.Scheme = ""
	parsed.Host = ""
	return parsed.RequestURI(), nil
}
