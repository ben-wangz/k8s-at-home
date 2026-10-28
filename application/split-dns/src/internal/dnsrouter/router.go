package dnsrouter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/miekg/dns"
	"split-dns/internal/config"
)

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

const maxUDPResponseSize = 1232

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
		domain = dns.CanonicalName(domain)
		router.rules = append(router.rules, compiledRule{
			match: rule.Match, domain: domain, group: rule.UpstreamGroup,
			labels: dns.CountLabel(domain), ordinal: i,
		})
	}
	router.lifecycleCtx, router.cancel = context.WithCancel(context.Background())
	return router, nil
}

func (r *Router) Close() {
	r.cancel()
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
		_ = writeResponse(writer, request, response)
		r.record(groupName, response.Rcode, source, started)
		return
	}
	question := request.Question[0]
	name, err := normalizeQuestionName(question.Name)
	if err != nil {
		response.Rcode = dns.RcodeFormatError
		_ = writeResponse(writer, request, response)
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
	if err := writeResponse(writer, request, response); err != nil {
		r.logger.Debug("DNS response write failed", "group", groupName, "error", errorClass(err))
	}
	r.record(groupName, response.Rcode, source, started)
}

func (r *Router) selectGroup(name string) string {
	name = dns.CanonicalName(name)
	best := compiledRule{labels: -1, ordinal: len(r.rules)}
	for _, rule := range r.rules {
		matches := name == rule.domain
		if rule.match == "suffix" {
			matches = dns.CompareDomainName(name, rule.domain) == rule.labels
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

func normalizeQuestionName(name string) (string, error) {
	if _, ok := dns.IsDomainName(name); !ok {
		return "", errors.New("invalid DNS question name")
	}
	return dns.CanonicalName(name), nil
}

func writeResponse(writer dns.ResponseWriter, request, response *dns.Msg) error {
	localAddress := writer.LocalAddr()
	if localAddress == nil || localAddress.Network() != "udp" {
		return writer.WriteMsg(response)
	}
	limit := dns.MinMsgSize
	if opt := request.IsEdns0(); opt != nil && int(opt.UDPSize()) > limit {
		limit = int(opt.UDPSize())
	}
	if limit > maxUDPResponseSize {
		limit = maxUDPResponseSize
	}
	message := response.Copy()
	message.Truncate(limit)
	return writer.WriteMsg(message)
}

func (r *Router) resolve(ctx context.Context, group *runtimeGroup, request *dns.Msg) (*dns.Msg, error) {
	if group.settings.Type == "forward" {
		return r.resolveForward(ctx, group, request)
	}
	return r.resolveDoH(ctx, group, request)
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
