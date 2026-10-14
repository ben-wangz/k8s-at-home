package environment

import "testing"

func TestRegistryS3TLSMaterialValidatesFixtureServiceName(t *testing.T) {
	const dnsName = "registry-s3.develop.svc.cluster.local"
	material, err := newRegistryS3TLSMaterial("t0123456789abcd", dnsName)
	if err != nil {
		t.Fatalf("newRegistryS3TLSMaterial: %v", err)
	}
	if err := validateRegistryS3TLSMaterial(material, dnsName); err != nil {
		t.Fatalf("validateRegistryS3TLSMaterial: %v", err)
	}
	if err := validateRegistryS3TLSMaterial(material, "other.develop.svc.cluster.local"); err == nil {
		t.Fatal("validateRegistryS3TLSMaterial accepted a different service DNS name")
	}
}

func TestDigestRegistryS3FixtureCoversCredentialsAndCertificates(t *testing.T) {
	credentials := map[string][]byte{"AWS_ACCESS_KEY_ID": []byte("access"), "AWS_SECRET_ACCESS_KEY": []byte("secret")}
	tlsMaterial := map[string][]byte{"ca.crt": []byte("ca"), "tls.crt": []byte("certificate"), "tls.key": []byte("key")}
	first := digestRegistryS3Fixture(credentials, tlsMaterial)
	if first != digestRegistryS3Fixture(credentials, tlsMaterial) {
		t.Fatal("registry S3 fixture digest is not deterministic")
	}
	changedCredentials := map[string][]byte{"AWS_ACCESS_KEY_ID": []byte("different"), "AWS_SECRET_ACCESS_KEY": []byte("secret")}
	if first == digestRegistryS3Fixture(changedCredentials, tlsMaterial) {
		t.Fatal("registry S3 fixture digest ignored credential changes")
	}
	changedTLS := map[string][]byte{"ca.crt": []byte("ca"), "tls.crt": []byte("certificate"), "tls.key": []byte("different")}
	if first == digestRegistryS3Fixture(credentials, changedTLS) {
		t.Fatal("registry S3 fixture digest ignored TLS changes")
	}
}
