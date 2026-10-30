package app

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"registry-hub/internal/inventory"
	"strconv"
	"strings"
	"time"
)

type diagnosticCursor struct {
	Query      string    `json:"query"`
	Storage    string    `json:"storage,omitempty"`
	Phase      int       `json:"phase"`
	SnapshotID string    `json:"snapshotID,omitempty"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

type diagnosticPage[T any] struct {
	Items       []T       `json:"items"`
	NextToken   string    `json:"nextToken,omitempty"`
	ObservedAt  time.Time `json:"observedAt"`
	Consistency string    `json:"consistency"`
	Scanned     int       `json:"scanned"`
}

func (s *Service) diagnosticQuery(request *http.Request, allowed ...string) (url.Values, diagnosticCursor, int, error) {
	values, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil || !onlyQueryKeys(values, append(allowed, "limit", "token")...) {
		return nil, diagnosticCursor{}, 0, inventory.ErrInvalid
	}
	for _, v := range values {
		if len(v) != 1 || v[0] == "" || len(v[0]) > 8192 || strings.ContainsAny(v[0], "\x00\r\n") {
			return nil, diagnosticCursor{}, 0, inventory.ErrInvalid
		}
	}
	limit := 32
	if raw := values.Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 32 {
			return nil, diagnosticCursor{}, 0, inventory.ErrInvalid
		}
	}
	clone := url.Values{}
	for key, value := range values {
		if key != "token" {
			clone[key] = value
		}
	}
	filters := request.URL.Path + "?" + clone.Encode()
	cursor := diagnosticCursor{Query: filters, ExpiresAt: time.Now().UTC().Add(30 * time.Minute)}
	if token := values.Get("token"); token != "" {
		if err := s.decodeDiagnosticToken(token, filters, &cursor); err != nil {
			return nil, cursor, 0, err
		}
	}
	return values, cursor, limit, nil
}

func (s *Service) diagnosticCipher() (cipher.AEAD, error) {
	key := sha256.Sum256(append([]byte("registry-hub-diagnostics-v1\x00"), s.cursorKey...))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (s *Service) diagnosticToken(cursor diagnosticCursor) string {
	aead, err := s.diagnosticCipher()
	if err != nil {
		return ""
	}
	data, err := json.Marshal(cursor)
	if err != nil {
		return ""
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(aead.Seal(nonce, nonce, data, nil))
}

func (s *Service) decodeDiagnosticToken(token, query string, cursor *diagnosticCursor) error {
	aead, err := s.diagnosticCipher()
	if err != nil {
		return inventory.ErrInvalid
	}
	data, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(data) < aead.NonceSize() {
		return inventory.ErrInvalid
	}
	data, err = aead.Open(nil, data[:aead.NonceSize()], data[aead.NonceSize():], nil)
	if err != nil || json.Unmarshal(data, cursor) != nil || cursor.Query != query {
		return inventory.ErrInvalid
	}
	if !cursor.ExpiresAt.After(time.Now()) {
		return inventory.ErrExpired
	}
	return nil
}
