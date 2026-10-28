package dnsrouter

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/miekg/dns"
)

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
