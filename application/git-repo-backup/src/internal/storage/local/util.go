package local

import "crypto/rand"

// randomSuffix returns 8 hex characters for unique trash names.
func randomSuffix() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "fallback"
	}
	const hex = "0123456789abcdef"
	out := make([]byte, 8)
	for i, v := range b {
		out[i*2] = hex[v>>4]
		out[i*2+1] = hex[v&0xf]
	}
	return string(out)
}
