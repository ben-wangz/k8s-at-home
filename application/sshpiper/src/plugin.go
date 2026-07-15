package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"
	"github.com/tg123/sshpiper/libplugin"
)

type authMode string

const (
	authModePassword authMode = "password"
	authModeKey      authMode = "key"
)

type hostKeyPolicy string

const (
	hostKeyPolicyStrict   hostKeyPolicy = "strict"
	hostKeyPolicyTOFU     hostKeyPolicy = "tofu"
	hostKeyPolicyInsecure hostKeyPolicy = "insecure"
)

type routeAuthConfig struct {
	authMode               string
	allowedTargets         string
	hostKeyPolicy          string
	knownHostsPath         string
	authorizedKeysPath     string
	upstreamPrivateKeyPath string
}

type routeAuthPlugin struct {
	mode           authMode
	policy         hostKeyPolicy
	matcher        targetMatcher
	knownHostsPath string
	authorizedKeys string
	privateKeyPath string
	knownHostsMu   sync.Mutex
}

type route struct {
	upstreamUser string
	target       string
	port         int
}

type targetMatcher struct {
	exact  map[string]struct{}
	suffix []string
}

func newRouteAuthPlugin(cfg routeAuthConfig) (*routeAuthPlugin, error) {
	mode := authMode(strings.TrimSpace(cfg.authMode))
	switch mode {
	case authModePassword, authModeKey:
	default:
		return nil, fmt.Errorf("invalid auth mode: %s", cfg.authMode)
	}

	policy := hostKeyPolicy(strings.TrimSpace(cfg.hostKeyPolicy))
	switch policy {
	case hostKeyPolicyStrict, hostKeyPolicyTOFU, hostKeyPolicyInsecure:
	default:
		return nil, fmt.Errorf("invalid host key policy: %s", cfg.hostKeyPolicy)
	}

	matcher := parseAllowedTargets(cfg.allowedTargets)
	if len(matcher.exact) == 0 && len(matcher.suffix) == 0 {
		return nil, errors.New("allowed targets must not be empty")
	}

	plugin := &routeAuthPlugin{
		mode:           mode,
		policy:         policy,
		matcher:        matcher,
		knownHostsPath: cfg.knownHostsPath,
		authorizedKeys: cfg.authorizedKeysPath,
		privateKeyPath: cfg.upstreamPrivateKeyPath,
	}

	if plugin.mode == authModeKey {
		if _, err := os.ReadFile(plugin.authorizedKeys); err != nil {
			return nil, fmt.Errorf("read authorized_keys: %w", err)
		}
		if _, err := os.ReadFile(plugin.privateKeyPath); err != nil {
			return nil, fmt.Errorf("read upstream private key: %w", err)
		}
	}

	if plugin.policy == hostKeyPolicyStrict {
		if _, err := os.ReadFile(plugin.knownHostsPath); err != nil {
			return nil, fmt.Errorf("read known_hosts for strict policy: %w", err)
		}
	}

	return plugin, nil
}

func (p *routeAuthPlugin) CreateConfig() *libplugin.SshPiperPluginConfig {
	return &libplugin.SshPiperPluginConfig{
		NextAuthMethodsCallback:     p.supportedMethods,
		PasswordCallback:            p.passwordAuth,
		PublicKeyCallback:           p.publicKeyAuth,
		UpstreamAuthFailureCallback: p.upstreamAuthFailure,
		PipeStartCallback:           p.pipeStart,
		PipeErrorCallback:           p.pipeError,
		PipeCreateErrorCallback:     p.pipeCreateError,
		VerifyHostKeyCallback:       p.verifyHostKey,
	}
}

func (p *routeAuthPlugin) upstreamAuthFailure(conn libplugin.ConnMetadata, method string, err error, allowmethods []string) {
	log.Errorf("upstreamAuthFailure: user=%q method=%q err=%v allowed=%q", conn.User(), method, err, allowmethods)
}

func (p *routeAuthPlugin) pipeStart(conn libplugin.ConnMetadata) {
	log.Infof("pipeStart: user=%q remote=%q", conn.User(), conn.RemoteAddr())
}

func (p *routeAuthPlugin) pipeError(conn libplugin.ConnMetadata, err error) {
	log.Errorf("pipeError: user=%q remote=%q err=%v", conn.User(), conn.RemoteAddr(), err)
}

func (p *routeAuthPlugin) pipeCreateError(remoteAddr string, err error) {
	log.Errorf("pipeCreateError: remote=%q err=%v", remoteAddr, err)
}

func (p *routeAuthPlugin) supportedMethods(conn libplugin.ConnMetadata) ([]string, error) {
	if p.mode == authModeKey {
		return []string{"publickey"}, nil
	}

	return []string{"password"}, nil
}

func (p *routeAuthPlugin) passwordAuth(conn libplugin.ConnMetadata, password []byte) (*libplugin.Upstream, error) {
	if p.mode != authModePassword {
		return nil, errors.New("password auth disabled")
	}

	r, err := p.parseAndValidate(conn)
	if err != nil {
		log.Errorf("passwordAuth: parse failed for user=%q: %v", conn.User(), err)
		return nil, err
	}

	return &libplugin.Upstream{
		Host:          r.target,
		Port:          int32(r.port),
		UserName:      r.upstreamUser,
		IgnoreHostKey: p.policy == hostKeyPolicyInsecure,
		Auth:          libplugin.CreatePasswordAuth(password),
	}, nil
}

func (p *routeAuthPlugin) publicKeyAuth(conn libplugin.ConnMetadata, key []byte) (*libplugin.Upstream, error) {
	if p.mode != authModeKey {
		return nil, errors.New("public key auth disabled")
	}

	r, err := p.parseAndValidate(conn)
	if err != nil {
		log.Errorf("publicKeyAuth: parse failed for user=%q: %v", conn.User(), err)
		return nil, err
	}

	authorizedKeys, err := os.ReadFile(p.authorizedKeys)
	if err != nil {
		return nil, fmt.Errorf("read authorized_keys: %w", err)
	}

	matched, err := isAuthorizedKey(authorizedKeys, key)
	if err != nil {
		return nil, err
	}
	if !matched {
		return nil, errors.New("unauthorized public key")
	}

	privateKey, err := os.ReadFile(p.privateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("read upstream private key: %w", err)
	}

	return &libplugin.Upstream{
		Host:          r.target,
		Port:          int32(r.port),
		UserName:      r.upstreamUser,
		IgnoreHostKey: p.policy == hostKeyPolicyInsecure,
		Auth:          libplugin.CreatePrivateKeyAuth(privateKey),
	}, nil
}

func (p *routeAuthPlugin) parseAndValidate(conn libplugin.ConnMetadata) (route, error) {
	rawUser := conn.User()
	r, err := parseRoute(rawUser)
	if err != nil {
		return route{}, err
	}

	if !p.matcher.Match(r.target) {
		return route{}, fmt.Errorf("target not allowed: %s", r.target)
	}

	return r, nil
}

func init() {
	log.SetFormatter(&log.TextFormatter{FullTimestamp: true})
}
