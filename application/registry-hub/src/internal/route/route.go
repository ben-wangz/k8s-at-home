package route

import (
	"encoding/hex"
	"errors"
	"net/url"
	"strings"
)

var ErrInvalidPath = errors.New("invalid registry path")

type Endpoint string

const (
	EndpointV2        Endpoint = "v2"
	EndpointManifest  Endpoint = "manifest"
	EndpointBlob      Endpoint = "blob"
	EndpointUpload    Endpoint = "upload"
	EndpointTags      Endpoint = "tags"
	EndpointCatalog   Endpoint = "catalog"
	EndpointReferrers Endpoint = "referrers"
)

type Route struct {
	Repository  string
	Internal    string
	Reference   string
	Endpoint    Endpoint
	UploadID    string
	RawPath     string
	Upstream    string
	RemoteRepo  string
	HasUpstream bool
}

func ParseURLPath(requestURI string) (string, error) {
	u, err := url.ParseRequestURI(requestURI)
	if err != nil || u.RawPath == "" && u.Path == "" {
		return "", ErrInvalidPath
	}
	raw := u.EscapedPath()
	decoded, err := url.PathUnescape(raw)
	if err != nil || strings.Contains(decoded, `\`) || strings.Contains(strings.ToLower(raw), "%2f") || strings.Contains(strings.ToLower(raw), "%5c") || strings.Contains(strings.ToLower(decoded), "%2f") || strings.Contains(strings.ToLower(decoded), "%5c") {
		return "", ErrInvalidPath
	}
	segments := strings.Split(strings.TrimPrefix(decoded, "/"), "/")
	for i, segment := range segments {
		if segment == "" && i == len(segments)-1 && strings.HasSuffix(decoded, "/") {
			continue
		}
		if segment == "" || segment == "." || segment == ".." || strings.ContainsAny(segment, "\x00\r\n") {
			return "", ErrInvalidPath
		}
	}
	return decoded, nil
}

func Parse(requestURI string) (Route, error) {
	path, err := ParseURLPath(requestURI)
	if err != nil {
		return Route{}, err
	}
	if path == "/v2/" || path == "/v2" {
		return Route{Endpoint: EndpointV2, RawPath: path}, nil
	}
	if path == "/v2/_catalog" {
		return Route{Endpoint: EndpointCatalog, RawPath: path}, nil
	}
	if !strings.HasPrefix(path, "/v2/") {
		return Route{}, ErrInvalidPath
	}
	parts := strings.Split(strings.TrimPrefix(path, "/v2/"), "/")
	if len(parts) > 0 && parts[len(parts)-1] == "" && strings.HasSuffix(path, "/") {
		parts = parts[:len(parts)-1]
	}
	if len(parts) < 2 {
		return Route{}, ErrInvalidPath
	}
	last := len(parts) - 1
	r := Route{RawPath: path}
	switch {
	case last >= 1 && parts[last-1] == "manifests":
		r.Endpoint, r.Reference = EndpointManifest, parts[last]
		parts = parts[:last-1]
	case last >= 2 && parts[last-2] == "blobs" && parts[last-1] == "uploads":
		r.Endpoint, r.UploadID = EndpointUpload, parts[last]
		parts = parts[:last-2]
	case last >= 1 && parts[last-1] == "blobs" && parts[last] == "uploads":
		r.Endpoint = EndpointUpload
		parts = parts[:last-1]
	case last >= 1 && parts[last-1] == "blobs":
		r.Endpoint, r.Reference = EndpointBlob, parts[last]
		parts = parts[:last-1]
	case parts[last] == "blobs" && last+1 < len(parts):
		return Route{}, ErrInvalidPath
	case last >= 1 && parts[last-1] == "tags" && parts[last] == "list":
		r.Endpoint = EndpointTags
		parts = parts[:last-1]
	case last >= 1 && parts[last-1] == "referrers":
		r.Endpoint, r.Reference = EndpointReferrers, parts[last]
		parts = parts[:last-1]
	default:
		return Route{}, ErrInvalidPath
	}
	if len(parts) == 0 {
		return Route{}, ErrInvalidPath
	}
	r.Repository = strings.Join(parts, "/")
	if err := ValidateRepository(r.Repository); err != nil {
		return Route{}, err
	}
	if r.Endpoint == EndpointManifest {
		if !ValidReference(r.Reference) {
			return Route{}, ErrInvalidPath
		}
	} else if r.Endpoint == EndpointBlob || r.Endpoint == EndpointReferrers {
		if !ValidDigest(r.Reference) {
			return Route{}, ErrInvalidPath
		}
	}
	r.Internal, err = InternalName(r.Repository)
	if err != nil {
		return Route{}, err
	}
	if index := strings.IndexByte(r.Repository, '/'); index >= 0 {
		r.Upstream = r.Repository[:index]
		r.RemoteRepo = r.Repository[index+1:]
		if err := ValidateAuthority(r.Upstream); err != nil {
			return Route{}, err
		}
		r.HasUpstream = true
	}
	return r, nil
}

func InternalName(repository string) (string, error) {
	if strings.Contains(repository, "/") {
		parts := strings.Split(repository, "/")
		if len(parts) < 2 {
			return "", ErrInvalidPath
		}
		for _, part := range parts[1:] {
			if !ordinary.MatchString(part) {
				return "", ErrInvalidPath
			}
		}
		first := parts[0]
		if ordinary.MatchString(first) && !strings.HasPrefix(first, "e") && first != "localhost" && !strings.ContainsAny(first, ".:") {
			if len(repository) > 255 {
				return "", ErrInvalidPath
			}
			return repository, nil
		}
		var encoded strings.Builder
		encoded.WriteByte('e')
		for _, b := range []byte(first) {
			if b >= 'a' && b <= 'z' || b >= '0' && b <= '9' {
				encoded.WriteByte(b)
			} else {
				encoded.WriteByte('_')
				encoded.WriteString(hex.EncodeToString([]byte{b}))
			}
		}
		parts[0] = encoded.String()
		internal := strings.Join(parts, "/")
		if len(internal) > 255 {
			return "", ErrInvalidPath
		}
		return internal, nil
	}
	if !ordinary.MatchString(repository) || len(repository) > 255 {
		return "", ErrInvalidPath
	}
	return repository, nil
}

func DecodeInternalName(internal string) (string, bool) {
	first, remainder, found := strings.Cut(internal, "/")
	if !found || !strings.HasPrefix(first, "e") || len(first) == 1 {
		return internal, false
	}
	encoded := first[1:]
	var decoded []byte
	for i := 0; i < len(encoded); {
		if encoded[i] != '_' {
			decoded = append(decoded, encoded[i])
			i++
			continue
		}
		if i+2 >= len(encoded) {
			return internal, false
		}
		pair, err := hex.DecodeString(encoded[i+1 : i+3])
		if err != nil {
			return internal, false
		}
		decoded = append(decoded, pair[0])
		i += 3
	}
	logical := string(decoded) + "/" + remainder
	encodedLogical, err := InternalName(logical)
	if err != nil || encodedLogical != internal {
		return internal, false
	}
	return logical, true
}
