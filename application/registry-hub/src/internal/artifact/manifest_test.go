package artifact

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseManifestValidatesDescriptorPlatformAndPreservesExtensions(t *testing.T) {
	for _, test := range []struct {
		name     string
		platform json.RawMessage
		wantErr  string
	}{
		{
			name:     "valid platform with extension",
			platform: json.RawMessage(`{"architecture":"unknown","os":"unknown","os.version":"v1","os.features":["feature-a"],"variant":"v2","x-fixture":{"keep":true}}`),
		},
		{name: "non-object", platform: json.RawMessage(`"linux/amd64"`), wantErr: "must be an object"},
		{name: "null", platform: json.RawMessage(`null`), wantErr: "must be an object"},
		{name: "missing architecture", platform: json.RawMessage(`{"os":"linux"}`), wantErr: "missing architecture"},
		{name: "non-string architecture", platform: json.RawMessage(`{"architecture":1,"os":"linux"}`), wantErr: "architecture must be a string"},
		{name: "non-string os version", platform: json.RawMessage(`{"architecture":"amd64","os":"linux","os.version":1}`), wantErr: "os.version must be a string"},
		{name: "non-string feature", platform: json.RawMessage(`{"architecture":"amd64","os":"linux","os.features":[1]}`), wantErr: "os.features must be an array of strings"},
		{name: "non-string variant", platform: json.RawMessage(`{"architecture":"amd64","os":"linux","variant":true}`), wantErr: "variant must be a string"},
	} {
		t.Run(test.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{
				"schemaVersion": 2,
				"mediaType":     "application/vnd.oci.image.index.v1+json",
				"manifests": []any{map[string]any{
					"mediaType": "application/vnd.oci.image.manifest.v1+json",
					"digest":    CalculateSHA256([]byte("child")),
					"size":      5,
					"platform":  test.platform,
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			manifest, err := ParseManifest(body, "application/vnd.oci.image.index.v1+json", 10, 1<<20)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("ParseManifest error = %v, want containing %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseManifest: %v", err)
			}
			var fields map[string]json.RawMessage
			if manifest.Manifests == nil || len(*manifest.Manifests) != 1 {
				t.Fatalf("parsed index descriptors = %#v, want one descriptor", manifest.Manifests)
			}
			if err := json.Unmarshal((*manifest.Manifests)[0].Platform, &fields); err != nil {
				t.Fatalf("decode preserved platform: %v", err)
			}
			if string(fields["x-fixture"]) != `{"keep":true}` {
				t.Fatalf("unknown platform extension changed or disappeared: %s", fields["x-fixture"])
			}
		})
	}
}
