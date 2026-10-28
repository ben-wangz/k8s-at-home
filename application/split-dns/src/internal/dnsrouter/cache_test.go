package dnsrouter

import (
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
	"split-dns/internal/config"
)

func TestCachePreservesIndependentTTLsAndNegativeBound(t *testing.T) {
	settings := config.Cache{Enabled: true, MaxEntries: 4, MaxTTL: 300, NegativeMaxTTL: 30}
	cache := newResponseCache(settings)
	request := query("alias.example.")
	response := new(dns.Msg)
	response.SetReply(request)
	response.Answer = []dns.RR{
		&dns.CNAME{Hdr: dns.RR_Header{Name: "alias.example.", Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: 43}, Target: "target.example."},
		&dns.A{Hdr: dns.RR_Header{Name: "target.example.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 300}, A: net.ParseIP("192.0.2.8").To4()},
	}
	cache.store("default", request, response)
	key, _ := cacheKey("default", request)
	entry := cache.entries[key]
	entry.inserted = time.Now().Add(-1900 * time.Millisecond)
	cache.entries[key] = entry
	lookup := query("ALIAS.EXAMPLE.")
	lookup.Id++
	hit, ok := cache.lookup("default", lookup)
	if !ok || hit.Id != lookup.Id || hit.Answer[0].Header().Ttl != 41 || hit.Answer[1].Header().Ttl != 298 {
		t.Fatalf("cache hit=%v, ok=%t", hit, ok)
	}

	negative := new(dns.Msg)
	negative.SetReply(query("missing.example."))
	negative.Rcode = dns.RcodeNameError
	negative.Ns = []dns.RR{&dns.SOA{
		Hdr:     dns.RR_Header{Name: "example.", Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 300},
		Minttl:  60,
		Ns:      "ns.example.",
		Mbox:    "hostmaster.example.",
		Serial:  1,
		Refresh: 1,
		Retry:   1,
		Expire:  1,
	}}
	negativeTTL, cacheable := responseTTL(negative, 30, 300)
	if !cacheable || negativeTTL != 30 || negative.Ns[0].Header().Ttl != 30 {
		t.Fatalf("negative TTL=%d, cacheable=%t, SOA TTL=%d", negativeTTL, cacheable, negative.Ns[0].Header().Ttl)
	}
}

func TestCacheDoesNotExtendZeroTTL(t *testing.T) {
	cache := newResponseCache(config.Cache{Enabled: true, MaxEntries: 2, MaxTTL: 300, NegativeMaxTTL: 30})
	request := query("zero.example.")
	response := new(dns.Msg)
	response.SetReply(request)
	response.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: "zero.example.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 0}, A: net.ParseIP("192.0.2.9").To4()}}
	cache.store("default", request, response)
	if len(cache.entries) != 0 {
		t.Fatalf("zero-TTL answer was cached: %d entries", len(cache.entries))
	}
}
