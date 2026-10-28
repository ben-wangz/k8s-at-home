package dnsrouter

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/miekg/dns"
	"split-dns/internal/config"
)

const maxDNSMessageSize = 65535

type dohEndpoint struct {
	bootstrapIPs []string
	url          *url.URL
	client       *http.Client
	limits       config.UpstreamGroup
	group        string
	metrics      *Metrics
}

func newDoHEndpoint(settings config.Endpoint, limits config.UpstreamGroup, proxies map[string]config.Proxy) (*dohEndpoint, error) {
	endpointURL, err := url.Parse(settings.URL)
	if err != nil {
		return nil, err
	}
	host := endpointURL.Hostname()
	transport := &http.Transport{
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host},
		TLSHandshakeTimeout:   parseDuration(limits.TLSHandshakeTimeout),
		ResponseHeaderTimeout: parseDuration(limits.QueryTimeout),
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   4,
		MaxConnsPerHost:       8,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
		DialContext: (&net.Dialer{
			Timeout:   parseDuration(limits.ConnectTimeout),
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}
	if settings.CAFile != "" {
		pem, err := os.ReadFile(settings.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read CA bundle: %w", err)
		}
		roots, err := x509.SystemCertPool()
		if err != nil || roots == nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, errors.New("CA bundle contains no usable certificates")
		}
		transport.TLSClientConfig.RootCAs = roots
	}
	if limits.Egress.Mode == "proxy" {
		proxy := proxies[limits.Egress.ProxyRef]
		proxyURL, err := url.Parse(proxy.URL)
		if err != nil {
			return nil, fmt.Errorf("parse proxy URL: %w", err)
		}
		if proxy.UsernameEnv != "" {
			username, userOK := os.LookupEnv(proxy.UsernameEnv)
			password, passOK := os.LookupEnv(proxy.PasswordEnv)
			if !userOK || !passOK {
				return nil, errors.New("proxy authentication environment variables are not set")
			}
			proxyURL.User = url.UserPassword(username, password)
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	return &dohEndpoint{
		bootstrapIPs: append([]string(nil), settings.BootstrapIPs...),
		url:          endpointURL,
		client:       &http.Client{Transport: transport, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }},
		limits:       limits,
	}, nil
}

func parseDuration(value string) time.Duration {
	duration, _ := time.ParseDuration(value)
	return duration
}

func (r *Router) resolveDoH(ctx context.Context, group *runtimeGroup, request *dns.Msg) (*dns.Msg, error) {
	type result struct {
		message *dns.Msg
		err     error
	}
	results := make(chan result, len(group.endpoints))
	for _, endpoint := range group.endpoints {
		go func(endpoint *dohEndpoint) {
			message, err := endpoint.query(ctx, request)
			results <- result{message: message, err: err}
		}(endpoint)
	}
	var lastErr error
	for range group.endpoints {
		select {
		case result := <-results:
			if result.err == nil {
				return result.message, nil
			}
			lastErr = result.err
		case <-ctx.Done():
			select {
			case result := <-results:
				if result.err == nil {
					return result.message, nil
				}
				lastErr = result.err
			default:
			}
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, ctx.Err()
		}
	}
	if lastErr == nil {
		lastErr = errors.New("all DoH endpoints failed")
	}
	return nil, lastErr
}

func (e *dohEndpoint) query(ctx context.Context, request *dns.Msg) (*dns.Msg, error) {
	var lastErr error
	for attempt := 0; attempt < e.limits.MaxAttemptsPerEndpoint; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		response, err := e.attempt(ctx, request, attempt)
		if err == nil {
			if response.Rcode == dns.RcodeSuccess || response.Rcode == dns.RcodeNameError {
				e.metrics.Upstream.WithLabelValues(e.group, "doh", "success").Inc()
				return response, nil
			}
			e.metrics.Upstream.WithLabelValues(e.group, "doh", "servfail").Inc()
			lastErr = fmt.Errorf("DoH returned DNS rcode %s", dns.RcodeToString[response.Rcode])
		} else {
			e.metrics.Upstream.WithLabelValues(e.group, "doh", errorClass(err)).Inc()
			lastErr = err
		}
	}
	return nil, lastErr
}

func (e *dohEndpoint) attempt(ctx context.Context, request *dns.Msg, attempt int) (*dns.Msg, error) {
	query, err := request.Pack()
	if err != nil {
		return nil, err
	}
	endpointURL := *e.url
	requestHost := endpointURL.Host
	bootstrap := ""
	if len(e.bootstrapIPs) > 0 {
		bootstrap = e.bootstrapIPs[attempt%len(e.bootstrapIPs)]
	}
	if bootstrap != "" {
		port := endpointURL.Port()
		if port == "" {
			port = "443"
		}
		endpointURL.Host = net.JoinHostPort(bootstrap, port)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL.String(), bytes.NewReader(query))
	if err != nil {
		return nil, err
	}
	httpRequest.Host = requestHost
	httpRequest.Header.Set("Content-Type", "application/dns-message")
	httpRequest.Header.Set("Accept", "application/dns-message")
	response, err := e.client.Do(httpRequest)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("DoH returned HTTP status %d", response.StatusCode)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/dns-message" {
		return nil, errors.New("DoH response content type is not application/dns-message")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxDNSMessageSize+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxDNSMessageSize {
		return nil, errors.New("DoH response exceeded DNS message size limit")
	}
	message := new(dns.Msg)
	if err := message.Unpack(body); err != nil {
		return nil, fmt.Errorf("unpack DoH response: %w", err)
	}
	if !validDNSReply(request, message) {
		return nil, errors.New("DoH endpoint returned a mismatched DNS response")
	}
	message.Id = request.Id
	return message, nil
}

func validDNSReply(request, response *dns.Msg) bool {
	if response == nil || !response.Response || response.Id != request.Id || len(response.Question) != len(request.Question) {
		return false
	}
	for i, question := range response.Question {
		if !strings.EqualFold(question.Name, request.Question[i].Name) || question.Qtype != request.Question[i].Qtype || question.Qclass != request.Question[i].Qclass {
			return false
		}
	}
	return true
}
