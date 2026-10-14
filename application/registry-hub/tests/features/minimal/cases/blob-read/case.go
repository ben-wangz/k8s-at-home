package blobread

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

//go:embed case.json
var metadata embed.FS

func New() (testkit.Case, error) {
	definition, raw, err := testkit.LoadDefinition(metadata, "blob-read")
	if err != nil {
		return testkit.Case{}, err
	}
	return testkit.Case{Definition: definition, Raw: raw, Run: Run}, nil
}

func Run(ctx context.Context, environment testkit.CaseContext) error {
	dependency := environment.DependencyResults["blob-upload"]
	repository, err := support.DetailString(dependency, "repository")
	if err != nil {
		return err
	}
	digest, err := support.DetailString(dependency, "digest")
	if err != nil {
		return err
	}
	registry := support.NewRegistryClient(environment)
	content, err := support.ReadBlob(ctx, registry, repository, digest)
	if err != nil {
		return err
	}
	response, err := registry.Head(ctx, repository, "/blobs/"+digest, nil)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Length") != strconv.Itoa(len(content)) {
		response.Body.Close()
		return fmt.Errorf("blob HEAD did not report the stored length")
	}
	if body, readErr := io.ReadAll(response.Body); readErr != nil {
		response.Body.Close()
		return readErr
	} else if len(body) != 0 {
		response.Body.Close()
		return fmt.Errorf("blob HEAD returned a body")
	}
	response.Body.Close()

	ranges := []string{"bytes=0-0", "bytes=1-2", "bytes=-2", "bytes=1-"}
	for _, value := range ranges {
		response, err = registry.Request(ctx, http.MethodGet, registry.Endpoint(repository, "/blobs/"+digest), nil, map[string]string{"Range": value})
		if err != nil {
			return err
		}
		if response.StatusCode != http.StatusPartialContent {
			response.Body.Close()
			return fmt.Errorf("range %s returned %s", value, response.Status)
		}
		actual, readErr := testkit.ReadBody(response, int64(len(content)))
		if readErr != nil {
			return readErr
		}
		start, end, parseErr := parseRange(value, len(content))
		if parseErr != nil || !bytes.Equal(actual, content[start:end+1]) {
			return fmt.Errorf("range %s returned incorrect bytes", value)
		}
	}
	response, err = registry.Request(ctx, http.MethodGet, registry.Endpoint(repository, "/blobs/"+digest), nil, map[string]string{"Range": "bytes=999999-"})
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		response.Body.Close()
		return fmt.Errorf("unsatisfiable range returned %s", response.Status)
	}
	response.Body.Close()
	return support.WriteResult(environment, map[string]any{"repository": repository, "digest": digest, "size": len(content), "ranges": len(ranges)})
}

func parseRange(value string, size int) (int, int, error) {
	if !strings.HasPrefix(value, "bytes=") {
		return 0, 0, fmt.Errorf("unsupported range")
	}
	bounds := strings.SplitN(strings.TrimPrefix(value, "bytes="), "-", 2)
	if len(bounds) != 2 {
		return 0, 0, fmt.Errorf("unsupported range")
	}
	if bounds[0] == "" {
		count, err := strconv.Atoi(bounds[1])
		if err != nil || count < 1 || count > size {
			return 0, 0, fmt.Errorf("invalid suffix range")
		}
		return size - count, size - 1, nil
	}
	start, err := strconv.Atoi(bounds[0])
	if err != nil || start < 0 || start >= size {
		return 0, 0, fmt.Errorf("invalid range")
	}
	if bounds[1] == "" {
		return start, size - 1, nil
	}
	end, err := strconv.Atoi(bounds[1])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid range")
	}
	if err == nil {
		if start < 0 || end < start || end >= size {
			return 0, 0, fmt.Errorf("invalid range")
		}
		return start, end, nil
	}
	return 0, 0, fmt.Errorf("unsupported range")
}
