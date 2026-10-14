package environment

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"sort"
)

func validateUpstreamTLSMaterial(certificate, privateKey, caCertificate []byte) error {
	if len(certificate) == 0 || len(privateKey) == 0 || string(certificate) != string(caCertificate) {
		return fmt.Errorf("certificate, key, and CA material do not match")
	}
	pair, err := tls.X509KeyPair(certificate, privateKey)
	if err != nil {
		return err
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return err
	}
	if err := leaf.VerifyHostname("tls.registry-hub.test"); err != nil {
		return err
	}
	if err := leaf.VerifyHostname("github.com"); err != nil {
		return err
	}
	return nil
}

func validHTTPRule(rule upstreamTransportRule) bool {
	return rule.AllowHTTP && len(rule.Ports) == 1 && rule.Ports[0] == 80
}

func validHTTPAndTLSRule(rule upstreamTransportRule, caFile string) bool {
	return rule.AllowHTTP && rule.CAFile == caFile && len(rule.Ports) == 2 && rule.Ports[0] == 80 && rule.Ports[1] == 443
}

func validTLSRule(rule upstreamTransportRule, caFile string) bool {
	return !rule.AllowHTTP && rule.CAFile == caFile && len(rule.Ports) == 1 && rule.Ports[0] == 443
}

func upstreamProxyURL(release string) string {
	return "http://" + release + "-external-upstream-proxy." + Namespace + ".svc.cluster.local:8080"
}

func digestUpstreamConfigData(data map[string][]byte) string {
	hash := sha256.New()
	keys := make([]string, 0, len(data))
	for key := range data {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		_, _ = hash.Write([]byte(key))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(data[key])
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}
