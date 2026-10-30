package gc

import "testing"

func TestBackendClientAddressPreservesClientProtocolPortAndPath(t *testing.T) {
	for _, test := range []struct{ client, ip, expected string }{
		{"http://registry.test:5000", "10.0.0.2", "http://10.0.0.2:5000"},
		{"https://registry.test/prefix", "2001:db8::2", "https://[2001:db8::2]:443/prefix"},
		{"http://[2001:db8::1]:5001", "2001:db8::2", "http://[2001:db8::2]:5001"},
	} {
		actual, err := backendClientAddress(test.client, test.ip)
		if err != nil || actual != test.expected {
			t.Fatalf("client=%s ip=%s endpoint=%s err=%v", test.client, test.ip, actual, err)
		}
	}
	for _, test := range []struct{ client, ip string }{
		{"", "10.0.0.2"}, {"http://registry.test:5000", ""}, {"file://registry.test", "10.0.0.2"},
	} {
		if _, err := backendClientAddress(test.client, test.ip); err == nil {
			t.Fatalf("invalid address accepted: client=%s ip=%s", test.client, test.ip)
		}
	}
}
