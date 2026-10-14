package route

import "testing"

func TestParseRoutesUpstreamAuthorities(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		repository  string
		upstream    string
		remoteRepo  string
		hasUpstream bool
	}{
		{
			name:        "docker image",
			path:        "/v2/docker.io/library/busybox/manifests/latest",
			repository:  "docker.io/library/busybox",
			upstream:    "docker.io",
			remoteRepo:  "library/busybox",
			hasUpstream: true,
		},
		{
			name:        "ordinary first segment is still upstream",
			path:        "/v2/library/busybox/manifests/latest",
			repository:  "library/busybox",
			upstream:    "library",
			remoteRepo:  "busybox",
			hasUpstream: true,
		},
		{
			name:        "repository without slash has no upstream",
			path:        "/v2/busybox/manifests/latest",
			repository:  "busybox",
			hasUpstream: false,
		},
		{
			name:        "mirror-looking authority is not expanded",
			path:        "/v2/mirror/ghcr.io/team/image/manifests/latest",
			repository:  "mirror/ghcr.io/team/image",
			upstream:    "mirror",
			remoteRepo:  "ghcr.io/team/image",
			hasUpstream: true,
		},
		{
			name:        "local-looking authority is not expanded",
			path:        "/v2/local/registry.example/team/image/manifests/latest",
			repository:  "local/registry.example/team/image",
			upstream:    "local",
			remoteRepo:  "registry.example/team/image",
			hasUpstream: true,
		},
		{
			name:        "Docker registry host is not short-name expanded",
			path:        "/v2/registry-1.docker.io/library/busybox/manifests/latest",
			repository:  "registry-1.docker.io/library/busybox",
			upstream:    "registry-1.docker.io",
			remoteRepo:  "library/busybox",
			hasUpstream: true,
		},
		{
			name:        "host and port",
			path:        "/v2/registry.example:5000/team/image/manifests/latest",
			repository:  "registry.example:5000/team/image",
			upstream:    "registry.example:5000",
			remoteRepo:  "team/image",
			hasUpstream: true,
		},
		{
			name:        "bracketed IPv6 host and port",
			path:        "/v2/[2001:db8::1]:5000/team/image/manifests/latest",
			repository:  "[2001:db8::1]:5000/team/image",
			upstream:    "[2001:db8::1]:5000",
			remoteRepo:  "team/image",
			hasUpstream: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			routed, err := Parse(test.path)
			if err != nil {
				t.Fatalf("Parse(%q) returned error: %v", test.path, err)
			}
			if routed.Repository != test.repository || routed.Upstream != test.upstream || routed.RemoteRepo != test.remoteRepo || routed.HasUpstream != test.hasUpstream {
				t.Fatalf("unexpected route: %#v", routed)
			}
			decoded, encoded := DecodeInternalName(routed.Internal)
			if decoded != routed.Repository || !encoded && routed.Internal != routed.Repository {
				t.Fatalf("internal name %q does not round-trip to %q", routed.Internal, routed.Repository)
			}
		})
	}
}

func TestParseRejectsInvalidAuthoritiesAndEscapes(t *testing.T) {
	tests := []string{
		"/v2/2001:db8::1/team/image/manifests/latest",
		"/v2/registry.example:0/team/image/manifests/latest",
		"/v2/registry.example:65536/team/image/manifests/latest",
		"/v2/docker.io/library%2Fbusybox/manifests/latest",
		"/v2/docker.io/team%252Fimage/manifests/latest",
		"/v2/docker.io/team//image/manifests/latest",
		"/v2/docker.io/team%00image/manifests/latest",
		"/v2/docker.io/team%0Dimage/manifests/latest",
		"/v2/docker.io/team%5Cimage/manifests/latest",
		"/v2/docker.io/library/busybox/manifests/../latest",
		"/v2/docker.io/team/image/unknown/endpoint",
	}
	for _, path := range tests {
		t.Run(path, func(t *testing.T) {
			if _, err := Parse(path); err == nil {
				t.Fatalf("Parse(%q) accepted an invalid route", path)
			}
		})
	}
}

func TestParseMatchesEndpointSuffixAfterRepositoryWords(t *testing.T) {
	path := "/v2/docker.io/team/manifests/blobs/tags/referrers/image/manifests/latest"
	routed, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse(%q) returned error: %v", path, err)
	}
	if routed.Endpoint != EndpointManifest || routed.Repository != "docker.io/team/manifests/blobs/tags/referrers/image" || routed.Reference != "latest" {
		t.Fatalf("unexpected suffix route: %#v", routed)
	}
}

func TestValidateAuthorityPortAndUserInfoRules(t *testing.T) {
	for _, authority := range []string{
		"registry.example:",
		"[2001:db8::1]:",
		"registry.example:abc",
		"registry.example:0",
		"registry.example:65536",
		"user@registry.example",
		"registry.example/path",
		"registry.example?query",
		"registry.example\r\n",
		"2001:db8::1",
	} {
		t.Run(authority, func(t *testing.T) {
			if err := ValidateAuthority(authority); err == nil {
				t.Fatalf("ValidateAuthority(%q) accepted an invalid authority", authority)
			}
		})
	}
	for _, authority := range []string{"registry.example", "registry.example:5000", "[2001:db8::1]", "[2001:db8::1]:5000"} {
		t.Run(authority, func(t *testing.T) {
			if err := ValidateAuthority(authority); err != nil {
				t.Fatalf("ValidateAuthority(%q) returned error: %v", authority, err)
			}
		})
	}
}

func TestParseUploadRoutesBeforeBlobRoutes(t *testing.T) {
	for _, test := range []struct {
		path     string
		uploadID string
	}{
		{path: "/v2/docker.io/library/busybox/blobs/uploads/", uploadID: ""},
		{path: "/v2/docker.io/library/busybox/blobs/uploads/abc-123", uploadID: "abc-123"},
	} {
		t.Run(test.path, func(t *testing.T) {
			routed, err := Parse(test.path)
			if err != nil {
				t.Fatalf("Parse(%q) returned error: %v", test.path, err)
			}
			if routed.Endpoint != EndpointUpload || routed.UploadID != test.uploadID {
				t.Fatalf("unexpected upload route: %#v", routed)
			}
		})
	}
}

func TestInternalNameRoundTripsReservedAuthorities(t *testing.T) {
	for _, repository := range []string{
		"docker.io/library/busybox",
		"ecr.io/team/image",
		"registry.example:5000/team/image",
		"[2001:db8::1]:5000/team/image",
		"localhost/team/image",
	} {
		t.Run(repository, func(t *testing.T) {
			internal, err := InternalName(repository)
			if err != nil {
				t.Fatalf("InternalName(%q) returned error: %v", repository, err)
			}
			decoded, encoded := DecodeInternalName(internal)
			if decoded != repository || !encoded {
				t.Fatalf("InternalName(%q) = %q, DecodeInternalName = (%q, %t)", repository, internal, decoded, encoded)
			}
		})
	}
}

func TestValidDigestAlgorithm(t *testing.T) {
	for _, value := range []string{"sha256", "sha512", "sha256+custom_1"} {
		if !ValidDigestAlgorithm(value) {
			t.Fatalf("ValidDigestAlgorithm(%q) = false", value)
		}
	}
	for _, value := range []string{"", "SHA256", "sha/256", "sha256:", "sha256\n"} {
		if ValidDigestAlgorithm(value) {
			t.Fatalf("ValidDigestAlgorithm(%q) = true", value)
		}
	}
}
