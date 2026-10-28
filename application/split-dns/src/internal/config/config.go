package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

type Config struct {
	ListenAddress string `json:"listenAddress"`
	ListenPort    int    `json:"listenPort"`
	Metrics       struct {
		Address string `json:"address"`
		Port    int    `json:"port"`
		Enabled bool   `json:"enabled"`
	} `json:"metrics"`
	DNS DNS `json:"dns"`
}

type DNS struct {
	DefaultUpstreamGroup string                   `json:"defaultUpstreamGroup"`
	UpstreamGroups       map[string]UpstreamGroup `json:"upstreamGroups"`
	Proxies              map[string]Proxy         `json:"proxies"`
	Rules                []Rule                   `json:"rules"`
	Cache                Cache                    `json:"cache"`
}

type UpstreamGroup struct {
	Type                   string     `json:"type"`
	Servers                []string   `json:"servers"`
	Strategy               string     `json:"strategy"`
	Endpoints              []Endpoint `json:"endpoints"`
	Egress                 Egress     `json:"egress"`
	QueryTimeout           string     `json:"queryTimeout"`
	ConnectTimeout         string     `json:"connectTimeout"`
	TLSHandshakeTimeout    string     `json:"tlsHandshakeTimeout"`
	MaxAttemptsPerEndpoint int        `json:"maxAttemptsPerEndpoint"`
	MaxConcurrentRequests  int        `json:"maxConcurrentRequests"`
}

type Endpoint struct {
	URL          string   `json:"url"`
	BootstrapIPs []string `json:"bootstrapIPs"`
	CAFile       string   `json:"caFile"`
}

type Egress struct {
	Mode     string `json:"mode"`
	ProxyRef string `json:"proxyRef"`
}

type Proxy struct {
	URL         string `json:"url"`
	UsernameEnv string `json:"usernameEnv"`
	PasswordEnv string `json:"passwordEnv"`
}

type Rule struct {
	Match         string `json:"match"`
	Domain        string `json:"domain"`
	UpstreamGroup string `json:"upstreamGroup"`
}

type Cache struct {
	Enabled        bool `json:"enabled"`
	MaxEntries     int  `json:"maxEntries"`
	MaxTTL         int  `json:"maxTTL"`
	NegativeMaxTTL int  `json:"negativeMaxTTL"`
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		if err == nil {
			return Config{}, errors.New("parse config: expected one JSON object")
		}
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if c.ListenAddress == "" || c.ListenPort < 1 || c.ListenPort > 65535 {
		return errors.New("listenAddress and a valid listenPort are required")
	}
	if c.Metrics.Address == "" || c.Metrics.Port < 1 || c.Metrics.Port > 65535 {
		return errors.New("metrics address and a valid metrics port are required")
	}
	if c.ListenAddress == c.Metrics.Address && c.ListenPort == c.Metrics.Port {
		return errors.New("DNS and metrics listeners must use different addresses or ports")
	}
	if c.DNS.DefaultUpstreamGroup == "" {
		return errors.New("dns.defaultUpstreamGroup is required")
	}
	if _, ok := c.DNS.UpstreamGroups[c.DNS.DefaultUpstreamGroup]; !ok {
		return fmt.Errorf("default upstream group %q does not exist", c.DNS.DefaultUpstreamGroup)
	}
	if len(c.DNS.UpstreamGroups) == 0 {
		return errors.New("at least one upstream group is required")
	}
	for name, proxy := range c.DNS.Proxies {
		if !proxyNamePattern.MatchString(name) {
			return fmt.Errorf("proxy name %q must match %s", name, proxyNamePattern.String())
		}
		if err := validateProxy(proxy); err != nil {
			return fmt.Errorf("proxy %q: %w", name, err)
		}
	}
	for name, group := range c.DNS.UpstreamGroups {
		if !proxyNamePattern.MatchString(name) {
			return fmt.Errorf("upstream group name %q must match %s", name, proxyNamePattern.String())
		}
		if err := validateGroup(group, c.DNS.Proxies); err != nil {
			return fmt.Errorf("upstream group %q: %w", name, err)
		}
	}
	seenRules := make(map[string]struct{}, len(c.DNS.Rules))
	for i, rule := range c.DNS.Rules {
		domain, err := NormalizeDomain(rule.Domain)
		if err != nil {
			return fmt.Errorf("dns.rules[%d].domain: %w", i, err)
		}
		if rule.Match != "exact" && rule.Match != "suffix" {
			return fmt.Errorf("dns.rules[%d].match must be exact or suffix", i)
		}
		if _, ok := c.DNS.UpstreamGroups[rule.UpstreamGroup]; !ok {
			return fmt.Errorf("dns.rules[%d] references missing upstream group %q", i, rule.UpstreamGroup)
		}
		key := rule.Match + ":" + domain
		if _, exists := seenRules[key]; exists {
			return fmt.Errorf("duplicate DNS rule %q", key)
		}
		seenRules[key] = struct{}{}
	}
	if c.DNS.Cache.Enabled && (c.DNS.Cache.MaxEntries < 1 || c.DNS.Cache.MaxEntries > 16384 || c.DNS.Cache.MaxTTL < 1 || c.DNS.Cache.NegativeMaxTTL < 1) {
		return errors.New("enabled DNS cache requires maxEntries between 1 and 16384 and positive TTL limits")
	}
	if c.DNS.Cache.Enabled && (uint64(c.DNS.Cache.MaxTTL) > uint64(^uint32(0)) || uint64(c.DNS.Cache.NegativeMaxTTL) > uint64(^uint32(0))) {
		return errors.New("DNS cache TTL limits must fit in an unsigned 32-bit DNS TTL")
	}
	return nil
}

func NormalizeDomain(value string) (string, error) {
	name := strings.ToLower(strings.TrimSuffix(value, "."))
	if name == "" || len(name) > 253 {
		return "", errors.New("domain must be a non-empty ASCII DNS name")
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", errors.New("domain contains an invalid label")
		}
		for _, char := range label {
			if !(char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '-') {
				return "", errors.New("domain must use ASCII labels or punycode")
			}
		}
	}
	return name, nil
}
