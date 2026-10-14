package registrys3

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

func objectSnapshotDigest(objects map[string]string) string {
	keys := make([]string, 0, len(objects))
	for key := range objects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var buffer strings.Builder
	for _, key := range keys {
		buffer.WriteString(key)
		buffer.WriteByte(0)
		buffer.WriteString(objects[key])
		buffer.WriteByte(0)
	}
	digest := sha256.Sum256([]byte(buffer.String()))
	return hex.EncodeToString(digest[:])
}
