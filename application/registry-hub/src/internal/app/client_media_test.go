package app

import "testing"

func TestManifestAcceptDefaultsToAllMediaTypes(t *testing.T) {
	accept := manifestAcceptFor("")
	if !acceptsMedia(accept, "application/vnd.example.artifact.v1+json") {
		t.Fatalf("default manifest Accept does not allow generic OCI media type: %q", accept)
	}
}

func TestAcceptsMediaHonorsQualityAndWildcards(t *testing.T) {
	if acceptsMedia("application/json; q=0", "application/json") {
		t.Fatal("q=0 media type should not be accepted")
	}
	if !acceptsMedia("application/*;q=0.5", "application/vnd.example.artifact.v1+json") {
		t.Fatal("application wildcard should accept application media type")
	}
	if acceptsMedia("text/*", "application/vnd.example.artifact.v1+json") {
		t.Fatal("unmatched wildcard should not be accepted")
	}
}

func TestKnownAcceptRequiresSupportedManifestType(t *testing.T) {
	if knownAcceptSupported("text/plain") {
		t.Fatal("unsupported media type should not pass manifest discovery")
	}
	if knownAcceptSupported("application/json; q=0") {
		t.Fatal("zero-quality media type should not pass manifest discovery")
	}
	if !knownAcceptSupported("application/*;q=0.5") {
		t.Fatal("supported application wildcard should pass manifest discovery")
	}
	if !knownAcceptSupported("application/vnd.oci.image.index.v1+json") {
		t.Fatal("supported OCI index media type should pass manifest discovery")
	}
}
