package main

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"
	"github.com/tg123/sshpiper/libplugin"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
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
	if _, err := p.parseAndValidate(conn); err != nil {
		return nil, err
	}

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

func (p *routeAuthPlugin) loadKnownHosts() ([]byte, error) {
	if p.policy == hostKeyPolicyInsecure {
		return nil, nil
	}

	data, err := os.ReadFile(p.knownHostsPath)
	if err == nil {
		return data, nil
	}

	if errors.Is(err, os.ErrNotExist) {
		if p.policy == hostKeyPolicyStrict {
			return nil, err
		}
		return nil, nil
	}

	return nil, err
}

func (p *routeAuthPlugin) verifyHostKey(conn libplugin.ConnMetadata, hostname, netaddr string, key []byte) error {
	if p.policy == hostKeyPolicyInsecure {
		return nil
	}

	data, err := p.loadKnownHosts()
	if err != nil {
		if p.policy == hostKeyPolicyStrict {
			return err
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}

	if len(data) == 0 {
		if p.policy == hostKeyPolicyStrict {
			return errors.New("known_hosts is empty in strict mode")
		}
		return p.appendKnownHost(hostname, netaddr, key)
	}

	verifyErr := verifyHostKeyFromPath(p.knownHostsPath, hostname, netaddr, key)
	if verifyErr == nil {
		return nil
	}

	if p.policy == hostKeyPolicyStrict {
		return verifyErr
	}

	hostKeyErr := &knownhosts.KeyError{}
	if errors.As(verifyErr, &hostKeyErr) && len(hostKeyErr.Want) == 0 {
		return p.appendKnownHost(hostname, netaddr, key)
	}

	return verifyErr
}

func (p *routeAuthPlugin) appendKnownHost(hostname, netaddr string, key []byte) error {
	pub, err := ssh.ParsePublicKey(key)
	if err != nil {
		return err
	}

	hostPatterns := []string{hostname}
	host, port, splitErr := net.SplitHostPort(netaddr)
	if splitErr != nil {
		host = netaddr
	}

	if host != "" && host != hostname {
		if port != "" && port != "22" {
			hostPatterns = append(hostPatterns, knownhosts.Normalize(fmt.Sprintf("[%s]:%s", host, port)))
		} else {
			hostPatterns = append(hostPatterns, host)
		}
	}

	line := knownhosts.Line(hostPatterns, pub)
	if line == "" {
		return errors.New("failed to generate known_hosts entry")
	}

	p.knownHostsMu.Lock()
	defer p.knownHostsMu.Unlock()

	if err := os.MkdirAll(filepath.Dir(p.knownHostsPath), 0o755); err != nil {
		return err
	}

	current, err := os.ReadFile(p.knownHostsPath)
	if err == nil && len(current) > 0 {
		if verifyErr := verifyHostKeyFromPath(p.knownHostsPath, hostname, netaddr, key); verifyErr == nil {
			return nil
		}
	}

	f, err := os.OpenFile(p.knownHostsPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = f.WriteString(line + "\n")
	return err
}

func verifyHostKeyFromPath(path, hostname, netaddr string, key []byte) error {
	hostKeyCallback, err := knownhosts.New(path)
	if err != nil {
		return err
	}

	pub, err := ssh.ParsePublicKey(key)
	if err != nil {
		return err
	}

	addr, err := net.ResolveTCPAddr("tcp", netaddr)
	if err != nil {
		return err
	}

	return hostKeyCallback(hostname, addr, pub)
}

func (m targetMatcher) Match(target string) bool {
	if _, ok := m.exact[target]; ok {
		return true
	}

	for _, suffix := range m.suffix {
		if len(target) > len(suffix) && strings.HasSuffix(target, suffix) {
			return true
		}
	}

	return false
}

func parseAllowedTargets(raw string) targetMatcher {
	matcher := targetMatcher{exact: map[string]struct{}{}, suffix: []string{}}
	for _, token := range strings.Split(raw, ",") {
		t := strings.TrimSpace(token)
		if t == "" {
			continue
		}
		if strings.HasPrefix(t, "*.") && len(t) > 2 {
			matcher.suffix = append(matcher.suffix, t[1:])
			continue
		}
		matcher.exact[t] = struct{}{}
	}
	return matcher
}

func parseRoute(raw string) (route, error) {
	parts := strings.Split(raw, "+")
	if len(parts) != 2 && len(parts) != 3 {
		return route{}, fmt.Errorf("invalid username format")
	}

	r := route{
		upstreamUser: parts[0],
		target:       parts[1],
		port:         22,
	}

	if r.upstreamUser == "" || r.target == "" {
		return route{}, fmt.Errorf("invalid username format")
	}

	if len(parts) == 3 {
		port, err := strconv.Atoi(parts[2])
		if err != nil || port < 1 || port > 65535 {
			return route{}, fmt.Errorf("invalid port")
		}
		r.port = port
	}

	return r, nil
}

func isAuthorizedKey(authorizedKeys []byte, presented []byte) (bool, error) {
	presentedKey, err := ssh.ParsePublicKey(presented)
	if err != nil {
		return false, err
	}

	if _, ok := presentedKey.(*ssh.Certificate); ok {
		return false, errors.New("ssh certificates are not supported in key mode")
	}

	rest := authorizedKeys
	for len(rest) > 0 {
		parsedKey, _, _, next, err := ssh.ParseAuthorizedKey(rest)
		if err != nil {
			return false, err
		}
		rest = next

		candidate := parsedKey
		if _, ok := parsedKey.(*ssh.Certificate); ok {
			return false, errors.New("authorized_keys must not contain ssh certificates")
		}

		if subtle.ConstantTimeCompare(candidate.Marshal(), presentedKey.Marshal()) == 1 {
			return true, nil
		}
	}

	return false, nil
}

func init() {
	log.SetFormatter(&log.TextFormatter{FullTimestamp: true})
}
