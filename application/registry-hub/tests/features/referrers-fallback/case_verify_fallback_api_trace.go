package referrersfallback

import (
	"context"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

func verifyFallbackAPITrace(ctx context.Context, environment testkit.CaseContext, repository, tag, referrerDigest string, since time.Time) (map[string]any, error) {
	result, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "logs", "deployment/"+environment.Fullname+"-registry", "-c", "registry", "--since-time="+since.UTC().Format(time.RFC3339Nano), "--tail=20000")
	if err != nil {
		return nil, fmt.Errorf("read Distribution access logs: %w", err)
	}
	var events []fallbackAccessEvent
	for _, line := range strings.Split(string(result.Stdout), "\n") {
		method, methodOK := accessLogField(line, "http.request.method")
		requestURI, uriOK := accessLogField(line, "http.request.uri")
		statusText, statusOK := accessLogField(line, "http.response.status")
		if !methodOK || !uriOK || !statusOK {
			continue
		}
		parsedURI, parseErr := url.ParseRequestURI(requestURI)
		status, statusErr := strconv.Atoi(statusText)
		if parseErr != nil || statusErr != nil {
			continue
		}
		events = append(events, fallbackAccessEvent{method: method, path: parsedURI.EscapedPath(), status: status})
	}

	indexPath := "/v2/" + repository + "/manifests/" + tag
	referrerPath := "/v2/" + repository + "/manifests/" + referrerDigest
	initialMiss, indexPuts, indexReads, referrerDeletes := 0, 0, 0, 0
	firstPut, secondPut, firstReadAfterPut, secondReadAfterPut, deleteIndex := -1, -1, -1, -1, -1
	for index, event := range events {
		if event.path == indexPath {
			switch {
			case event.method == http.MethodGet && event.status == http.StatusNotFound:
				initialMiss++
			case event.method == http.MethodPut && event.status == http.StatusCreated:
				indexPuts++
				if firstPut < 0 {
					firstPut = index
				} else if secondPut < 0 {
					secondPut = index
				}
			case event.method == http.MethodGet && event.status == http.StatusOK:
				indexReads++
				if firstPut >= 0 && firstReadAfterPut < 0 {
					firstReadAfterPut = index
				} else if secondPut >= 0 && secondReadAfterPut < 0 {
					secondReadAfterPut = index
				}
			}
		}
		if event.path == referrerPath && event.method == http.MethodDelete && event.status == http.StatusAccepted {
			referrerDeletes++
			deleteIndex = index
		}
	}
	if initialMiss == 0 || indexPuts < 2 || indexReads < 2 || referrerDeletes == 0 || firstPut < 0 || secondPut < 0 || firstReadAfterPut < 0 || secondReadAfterPut < 0 || deleteIndex <= secondPut {
		return nil, fmt.Errorf("Distribution fallback trace incomplete: tag GET 404=%d PUT 201=%d GET 200=%d referrer DELETE 202=%d", initialMiss, indexPuts, indexReads, referrerDeletes)
	}
	return map[string]any{
		"fallbackTagPath": indexPath, "initialTagMisses": initialMiss,
		"fallbackIndexPuts201": indexPuts, "fallbackIndexReads200": indexReads,
		"referrerManifestDeletes202": referrerDeletes, "ordered": true,
	}, nil
}

func accessLogField(line, field string) (string, bool) {
	marker := field + "="
	start := strings.Index(line, marker)
	if start < 0 {
		return "", false
	}
	value := line[start+len(marker):]
	if strings.HasPrefix(value, "\"") {
		for index := 1; index < len(value); index++ {
			if value[index] == '\\' {
				index++
				continue
			}
			if value[index] == '"' {
				decoded, err := strconv.Unquote(value[:index+1])
				return decoded, err == nil
			}
		}
		return "", false
	}
	if end := strings.IndexAny(value, " \t"); end >= 0 {
		value = value[:end]
	}
	return value, value != ""
}

func hasDescriptor(descriptors []map[string]any, digest, artifactType string) bool {
	for _, descriptor := range descriptors {
		if testkit.String(descriptor["digest"]) == digest && testkit.String(descriptor["artifactType"]) == artifactType {
			return true
		}
	}
	return false
}

func fallbackTag(digest string) string {
	algorithm, encoded, _ := strings.Cut(digest, ":")
	return algorithm + "-" + encoded
}

func upstreamRequests(ctx context.Context, environment testkit.CaseContext) (float64, error) {
	response, err := support.AdminRequest(ctx, environment, http.MethodGet, "/metrics", nil, nil)
	if err != nil {
		return 0, err
	}
	metrics, err := testkit.ReadBody(response, 4<<20)
	if err != nil {
		return 0, err
	}
	var total float64
	for _, line := range strings.Split(string(metrics), "\n") {
		if strings.HasPrefix(line, "#") || !strings.HasPrefix(line, "registry_hub_upstream_requests_total") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, fmt.Errorf("upstream request metric is malformed")
		}
		value, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			return 0, fmt.Errorf("upstream request metric is not numeric")
		}
		total += value
	}
	return total, nil
}

func fallbackReferrerReads(ctx context.Context, environment testkit.CaseContext) (float64, error) {
	response, err := support.AdminRequest(ctx, environment, http.MethodGet, "/metrics", nil, nil)
	if err != nil {
		return 0, err
	}
	metrics, err := testkit.ReadBody(response, 4<<20)
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(metrics), "\n") {
		if strings.HasPrefix(line, "#") || !strings.HasPrefix(line, `registry_hub_referrer_reads_total{source="fallback"}`) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, fmt.Errorf("fallback referrer metric is malformed")
		}
		value, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			return 0, fmt.Errorf("fallback referrer metric is not numeric")
		}
		return value, nil
	}
	return 0, nil
}

func environmentVariable(key string) string {
	return strings.TrimSpace(os.Getenv(key))
}
