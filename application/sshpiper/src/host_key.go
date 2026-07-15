package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/tg123/sshpiper/libplugin"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

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
