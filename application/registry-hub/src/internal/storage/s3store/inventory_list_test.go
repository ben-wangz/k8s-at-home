package s3store

import (
	"encoding/xml"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

func (f *inventoryS3Fixture) list(w http.ResponseWriter, r *http.Request, etag func([]byte) string) {
	query := r.URL.Query()
	base := strings.TrimSuffix(r.URL.Path, "/") + "/"
	var keys []string
	for path := range f.objects {
		key := strings.TrimPrefix(path, base)
		if strings.HasPrefix(path, base) && strings.HasPrefix(key, query.Get("prefix")) && key > query.Get("continuation-token") {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	limit, _ := strconv.Atoi(query.Get("max-keys"))
	if limit < 1 {
		limit = 1000
	}
	result := struct {
		XMLName xml.Name `xml:"ListBucketResult"`
		Next    string   `xml:"NextContinuationToken,omitempty"`
		More    bool     `xml:"IsTruncated"`
		Items   []struct {
			Key  string `xml:"Key"`
			ETag string `xml:"ETag"`
			Size int    `xml:"Size"`
		} `xml:"Contents"`
	}{}
	if len(keys) > limit {
		keys = keys[:limit]
		result.Next, result.More = keys[len(keys)-1], true
	}
	for _, key := range keys {
		body := f.objects[base+key]
		result.Items = append(result.Items, struct {
			Key  string `xml:"Key"`
			ETag string `xml:"ETag"`
			Size int    `xml:"Size"`
		}{key, etag(body), len(body)})
	}
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(result)
}
