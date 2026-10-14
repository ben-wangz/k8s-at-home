package artifact

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"strings"
)

type Descriptor struct {
	MediaType    string            `json:"mediaType"`
	Digest       string            `json:"digest"`
	Size         int64             `json:"size"`
	Data         json.RawMessage   `json:"data,omitempty"`
	ArtifactType string            `json:"artifactType,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
	URLs         []string          `json:"urls,omitempty"`
	Embedded     []byte            `json:"-"`
	Platform     json.RawMessage   `json:"platform,omitempty"`
}

type Manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	ArtifactType  string            `json:"artifactType,omitempty"`
	Annotations   map[string]string `json:"annotations,omitempty"`
	Config        *Descriptor       `json:"config,omitempty"`
	Layers        []Descriptor      `json:"layers,omitempty"`
	Manifests     *[]Descriptor     `json:"manifests,omitempty"`
	Subject       *Descriptor       `json:"subject,omitempty"`
}

func ParseManifest(data []byte, responseType string, maxDescriptors int, maxEmbedded int64) (Manifest, error) {
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return manifest, fmt.Errorf("manifest JSON is invalid: %w", err)
	}
	if manifest.SchemaVersion != 2 {
		return manifest, errors.New("manifest schemaVersion must be 2")
	}
	contentType, _, err := mime.ParseMediaType(responseType)
	if err != nil || contentType == "" {
		return manifest, errors.New("manifest response has an invalid Content-Type")
	}
	if manifest.MediaType != "" && !strings.EqualFold(strings.TrimSpace(manifest.MediaType), contentType) {
		return manifest, errors.New("manifest mediaType does not match Content-Type")
	}
	if manifest.MediaType == "" {
		manifest.MediaType = contentType
	}
	count := len(manifest.Layers)
	if manifest.Config != nil {
		count++
	}
	if manifest.Manifests != nil {
		count += len(*manifest.Manifests)
	}
	if manifest.Subject != nil {
		count++
	}
	if count > maxDescriptors {
		return manifest, errors.New("manifest descriptor count exceeds limit")
	}
	var embedded int64
	descriptors := make([]*Descriptor, 0, count)
	if manifest.Config != nil {
		descriptors = append(descriptors, manifest.Config)
	}
	for index := range manifest.Layers {
		descriptors = append(descriptors, &manifest.Layers[index])
	}
	if manifest.Manifests != nil {
		for index := range *manifest.Manifests {
			descriptors = append(descriptors, &(*manifest.Manifests)[index])
		}
	}
	if manifest.Subject != nil {
		descriptors = append(descriptors, manifest.Subject)
	}
	for _, descriptor := range descriptors {
		if err := validateDescriptor(*descriptor); err != nil {
			return manifest, err
		}
		if len(descriptor.Data) > 0 && string(descriptor.Data) != "null" {
			var encoded string
			if err := json.Unmarshal(descriptor.Data, &encoded); err != nil {
				return manifest, errors.New("descriptor.data must be a Base64 string")
			}
			descriptor.Embedded, err = base64.StdEncoding.Strict().DecodeString(encoded)
			if err != nil {
				return manifest, errors.New("descriptor.data is not valid Base64")
			}
			embedded += int64(len(descriptor.Embedded))
			if embedded > maxEmbedded {
				return manifest, errors.New("manifest descriptor.data exceeds limit")
			}
			if int64(len(descriptor.Embedded)) != descriptor.Size {
				return manifest, errors.New("descriptor.data size mismatch")
			}
			if err := VerifyDigest(descriptor.Digest, descriptor.Embedded); err != nil {
				return manifest, fmt.Errorf("descriptor.data digest mismatch: %w", err)
			}
		}
	}
	return manifest, nil
}

func validateDescriptor(descriptor Descriptor) error {
	if descriptor.MediaType == "" || descriptor.Size < 0 {
		return errors.New("descriptor mediaType or size is invalid")
	}
	if _, err := Hasher(descriptor.Digest); err != nil {
		return err
	}
	if len(descriptor.Platform) > 0 {
		if err := validatePlatform(descriptor.Platform); err != nil {
			return err
		}
	}
	return nil
}

func validatePlatform(data json.RawMessage) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return errors.New("descriptor platform must be an object")
	}
	for _, name := range []string{"architecture", "os"} {
		value, ok := fields[name]
		if !ok {
			return fmt.Errorf("descriptor platform is missing %s", name)
		}
		var text string
		if err := json.Unmarshal(value, &text); err != nil {
			return fmt.Errorf("descriptor platform %s must be a string", name)
		}
	}
	for _, name := range []string{"os.version", "variant"} {
		value, ok := fields[name]
		if !ok {
			continue
		}
		var text string
		if err := json.Unmarshal(value, &text); err != nil {
			return fmt.Errorf("descriptor platform %s must be a string", name)
		}
	}
	if value, ok := fields["os.features"]; ok {
		var features []string
		if err := json.Unmarshal(value, &features); err != nil || features == nil {
			return errors.New("descriptor platform os.features must be an array of strings")
		}
	}
	return nil
}
