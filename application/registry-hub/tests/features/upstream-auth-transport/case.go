package upstreamauthtransport

import (
	"embed"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

//go:embed case.json fixture/main.go
var assets embed.FS

const (
	secretSuffix = "-external-upstream-auth"
	proxySuffix  = "-external-upstream-proxy"
	proxyImage   = "container-registry-mirror.internal.home.geekcity.tech:32443/docker.io/library/golang@sha256:397ecc648150dd585f1734dfcd8d1bb4746664e54464191d77ad0ecffaf38bdb"
)

type authCredential struct {
	Type     string `json:"type"`
	Username string `json:"username"`
	Password string `json:"password"`
	Token    string `json:"token"`
}

type transportRule struct {
	AllowHTTP bool     `json:"allowHTTP"`
	CAFile    string   `json:"caFile"`
	Ports     []uint16 `json:"ports"`
}

type proxyConfiguration struct {
	Default     string            `json:"default"`
	Authorities map[string]string `json:"authorities"`
}

func New() (testkit.Case, error) {
	definition, raw, err := testkit.LoadDefinition(assets, "feature-upstream-auth-transport")
	if err != nil {
		return testkit.Case{}, err
	}
	return testkit.Case{Definition: definition, Raw: raw, Run: Run}, nil
}
