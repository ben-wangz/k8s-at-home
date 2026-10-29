package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestInventoryEnumsRequireSingleExactValue(t *testing.T) {
	for name, values := range map[string]string{
		"category":       "solidified|cached_not_started|solidifying|failed|cancelled|cached_start_unknown|waiting_cache|not_scheduled|unknown",
		"cacheState":     "complete|partial|none|unknown",
		"executionState": "not_scheduled|idle|queued|running|retry_wait|waiting",
	} {
		t.Run(name, func(t *testing.T) {
			valid := strings.Split(values, "|")
			for _, value := range valid {
				r := httptest.NewRequest(http.MethodGet, "/api/v1/images?"+url.Values{name: {value}}.Encode(), nil)
				if _, err := parseInventoryQuery(r); err != nil {
					t.Fatalf("rejected valid %s=%s: %v", name, value, err)
				}
			}
			invalid := []string{"", "invalid", "|", "|" + valid[0], valid[0] + "|", strings.ToUpper(valid[0])}
			for n := 1; n < len(valid); n++ {
				invalid = append(invalid, valid[n-1]+"|"+valid[n])
			}
			for _, value := range invalid {
				r := httptest.NewRequest(http.MethodGet, "/api/v1/images?"+url.Values{name: {value}}.Encode(), nil)
				w := httptest.NewRecorder()
				(&Service{}).serveInventory(w, r, context.Background())
				if w.Code != http.StatusBadRequest {
					t.Fatalf("%s=%q: status=%d body=%s", name, value, w.Code, w.Body.String())
				}
			}
		})
	}
	if _, err := parseInventoryQuery(httptest.NewRequest(http.MethodGet, "/api/v1/images", nil)); err != nil {
		t.Fatalf("omitted filters: %v", err)
	}
}
