package dnsrouter

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
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

type Router struct {
	defaultGroup string
	groups       map[string]*runtimeGroup
	rules        []compiledRule
	cache        *responseCache
	metrics      *Metrics
	logger       *slog.Logger
	lifecycleCtx context.Context
	cancel       context.CancelFunc
}

type runtimeGroup struct {
	name      string
	settings  config.UpstreamGroup
	semaphore chan struct{}
	forward   []string
	endpoints []*dohEndpoint
}

type compiledRule struct {
	match   string
	domain  string
	group   string
	labels  int
	ordinal int
}

type dohEndpoint struct {
	bootstrapIPs []string
	url          *url.URL
	client       *http.Client
	limits       config.UpstreamGroup
	group        string
	metrics      *Metrics
}

func New(cfg config.Config, metrics *Metrics, logger *slog.Logger) (*Router, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	router := &Router{
		defaultGroup: cfg.DNS.DefaultUpstreamGroup,
		groups:       make(map[string]*runtimeGroup, len(cfg.DNS.UpstreamGroups)),
		cache:        newResponseCache(cfg.DNS.Cache),
		metrics:      metrics,
		logger:       logger,
	}
	for name, settings := range cfg.DNS.UpstreamGroups {
		group := &runtimeGroup{name: name, settings: settings, semaphore: make(chan struct{}, settings.MaxConcurrentRequests)}
		if settings.Type == "forward" {
			group.forward = append([]string(nil), settings.Servers...)
		} else {
			for _, endpoint := range settings.Endpoints {
				compiled, err := newDoHEndpoint(endpoint, settings, cfg.DNS.Proxies)
				if err != nil {
					return nil, fmt.Errorf("upstream group %q: %w", name, err)
				}
				compiled.group = name
				compiled.metrics = metrics
				group.endpoints = append(group.endpoints, compiled)
			}
		}
		router.groups[name] = group
	}
	for i, rule := range cfg.DNS.Rules {
		domain, _ := config.NormalizeDomain(rule.Domain)
		router.rules = append(router.rules, compiledRule{
			match: rule.Match, domain: domain, group: rule.UpstreamGroup,
			labels: len(strings.Split(domain, ".")), ordinal: i,
		})
	}
	router.lifecycleCtx, router.cancel = context.WithCancel(context.Background())
	return router, nil
}

func (r *Router) Close() {
	r.cancel()
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

func (r *Router) ServeDNS(writer dns.ResponseWriter, request *dns.Msg) {
	r.ServeDNSContext(context.Background(), writer, request)
}

func (r *Router) ServeDNSContext(clientCtx context.Context, writer dns.ResponseWriter, request *dns.Msg) {
	if clientCtx == nil {
		clientCtx = context.Background()
	}
	if clientCtx.Err() != nil || r.lifecycleCtx.Err() != nil {
		return
	}
	started := time.Now()
	groupName := r.defaultGroup
	source := "upstream"
	response := new(dns.Msg)
	response.SetReply(request)
	if len(request.Question) != 1 || request.Opcode != dns.OpcodeQuery {
		response.Rcode = dns.RcodeFormatError
		_ = writer.WriteMsg(response)
		r.record(groupName, response.Rcode, source, started)
		return
	}
	question := request.Question[0]
	name := ""
	var err error
	if question.Name != "." {
		name, err = config.NormalizeDomain(question.Name)
	}
	if err != nil {
		response.Rcode = dns.RcodeFormatError
		_ = writer.WriteMsg(response)
		r.record(groupName, response.Rcode, source, started)
		return
	}
	groupName = r.selectGroup(name)
	group := r.groups[groupName]
	if cached, ok := r.cache.lookup(groupName, request); ok {
		source = "cache"
		response = cached
		r.metrics.Cache.WithLabelValues("hit").Inc()
	} else {
		if r.cache.config.Enabled {
			r.metrics.Cache.WithLabelValues("miss").Inc()
		}
		parentCtx, cancelParent := context.WithCancel(r.lifecycleCtx)
		stopClientCancel := context.AfterFunc(clientCtx, cancelParent)
		ctx, cancel := context.WithTimeout(parentCtx, parseDuration(group.settings.QueryTimeout))
		defer cancel()
		defer cancelParent()
		defer stopClientCancel()
		select {
		case group.semaphore <- struct{}{}:
			defer func() { <-group.semaphore }()
		case <-ctx.Done():
			err = ctx.Err()
		}
		if err == nil {
			response, err = r.resolve(ctx, group, request)
		}
		if clientCtx.Err() != nil || r.lifecycleCtx.Err() != nil {
			r.record(groupName, dns.RcodeServerFailure, source, started)
			return
		}
		if err != nil || response == nil {
			response = new(dns.Msg)
			response.SetReply(request)
			response.Rcode = dns.RcodeServerFailure
			r.logger.Warn("DNS upstream request failed", "group", groupName, "error", errorClass(err))
		} else {
			r.cache.store(groupName, request, response)
		}
	}
	if clientCtx.Err() != nil || r.lifecycleCtx.Err() != nil {
		r.record(groupName, dns.RcodeServerFailure, source, started)
		return
	}
	if err := writer.WriteMsg(response); err != nil {
		r.logger.Debug("DNS response write failed", "group", groupName, "error", errorClass(err))
	}
	r.record(groupName, response.Rcode, source, started)
}

func (r *Router) selectGroup(name string) string {
	best := compiledRule{labels: -1, ordinal: len(r.rules)}
	for _, rule := range r.rules {
		matches := name == rule.domain
		if rule.match == "suffix" {
			matches = matches || strings.HasSuffix(name, "."+rule.domain)
		}
		if !matches || rule.labels < best.labels {
			continue
		}
		if rule.labels == best.labels && best.labels >= 0 {
			if rule.match != "exact" || best.match == "exact" {
				continue
			}
		}
		best = rule
	}
	if best.labels < 0 {
		return r.defaultGroup
	}
	return best.group
}

func (r *Router) resolve(ctx context.Context, group *runtimeGroup, request *dns.Msg) (*dns.Msg, error) {
	if group.settings.Type == "forward" {
		return r.resolveForward(ctx, group, request)
	}
	return r.resolveDoH(ctx, group, request)
}

func (r *Router) resolveForward(ctx context.Context, group *runtimeGroup, request *dns.Msg) (*dns.Msg, error) {
	var lastErr error
	for _, address := range group.forward {
		for _, network := range []string{"udp", "tcp"} {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			client := &dns.Client{Net: network, Timeout: time.Until(deadline(ctx))}
			response, _, err := client.ExchangeContext(ctx, request.Copy(), address)
			if err != nil {
				lastErr = err
				r.metrics.Upstream.WithLabelValues(group.name, network, "error").Inc()
				continue
			}
			if !validDNSReply(request, response) {
				lastErr = errors.New("upstream returned a mismatched DNS response")
				r.metrics.Upstream.WithLabelValues(group.name, network, "invalid").Inc()
				continue
			}
			if response.Truncated && network == "udp" {
				r.metrics.Upstream.WithLabelValues(group.name, network, "truncated").Inc()
				continue
			}
			if response.Rcode == dns.RcodeServerFailure {
				lastErr = errors.New("forward DNS upstream returned SERVFAIL")
				r.metrics.Upstream.WithLabelValues(group.name, network, "servfail").Inc()
				break
			}
			r.metrics.Upstream.WithLabelValues(group.name, network, "success").Inc()
			response.Id = request.Id
			return response, nil
		}
	}
	if lastErr == nil {
		lastErr = errors.New("all DNS upstreams failed")
	}
	return nil, lastErr
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

func deadline(ctx context.Context) time.Time {
	deadline, _ := ctx.Deadline()
	return deadline
}

func errorClass(err error) string {
	if err == nil {
		return "unknown"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return "timeout"
	}
	return "upstream_error"
}

func (r *Router) record(group string, rcode int, source string, started time.Time) {
	code, ok := dns.RcodeToString[rcode]
	if !ok {
		code = "OTHER"
	}
	r.metrics.Requests.WithLabelValues(group, code, source).Inc()
	r.metrics.Latency.WithLabelValues(group).Observe(time.Since(started).Seconds())
}

var _ dns.Handler = (*Router)(nil)

var _ interface {
	ServeDNSContext(context.Context, dns.ResponseWriter, *dns.Msg)
} = (*Router)(nil)
