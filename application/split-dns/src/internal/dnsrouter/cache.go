package dnsrouter

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	"split-dns/internal/config"
)

const maxCacheMessageSize = 8192

type cacheEntry struct {
	message  *dns.Msg
	inserted time.Time
	ttl      time.Duration
}

type responseCache struct {
	mu      sync.Mutex
	entries map[string]cacheEntry
	config  config.Cache
}

func newResponseCache(settings config.Cache) *responseCache {
	return &responseCache{entries: make(map[string]cacheEntry), config: settings}
}

func (c *responseCache) lookup(group string, request *dns.Msg) (*dns.Msg, bool) {
	if !c.config.Enabled {
		return nil, false
	}
	key, ok := cacheKey(group, request)
	if !ok {
		return nil, false
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, exists := c.entries[key]
	if !exists {
		return nil, false
	}
	if !now.Before(entry.inserted.Add(entry.ttl)) {
		delete(c.entries, key)
		return nil, false
	}
	message := entry.message.Copy()
	message.Id = request.Id
	if len(message.Question) == 1 && len(request.Question) == 1 {
		message.Question[0] = request.Question[0]
	}
	elapsed := now.Sub(entry.inserted)
	seconds := uint32((elapsed + time.Second - 1) / time.Second)
	decrementTTLs(message, seconds)
	return message, true
}

func (c *responseCache) store(group string, request, response *dns.Msg) {
	if !c.config.Enabled || response == nil || response.Truncated || response.Rcode != dns.RcodeSuccess && response.Rcode != dns.RcodeNameError {
		return
	}
	if response.Len() > maxCacheMessageSize {
		return
	}
	key, ok := cacheKey(group, request)
	if !ok {
		return
	}
	stored := response.Copy()
	maxTTL := uint32(c.config.MaxTTL)
	ttl, cacheable := responseTTL(stored, uint32(c.config.NegativeMaxTTL), maxTTL)
	if !cacheable || ttl == 0 {
		return
	}
	capTTLs(stored, maxTTL)
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[key]; !exists && len(c.entries) >= c.config.MaxEntries {
		c.evictOldest()
	}
	c.entries[key] = cacheEntry{message: stored, inserted: time.Now(), ttl: time.Duration(ttl) * time.Second}
}

func cacheKey(group string, request *dns.Msg) (string, bool) {
	if request == nil || len(request.Question) != 1 || request.Opcode != dns.OpcodeQuery || request.IsTsig() != nil {
		return "", false
	}
	var dnssecOK, checkingDisabled bool
	var udpSize uint16
	for _, extra := range request.Extra {
		opt, ok := extra.(*dns.OPT)
		if !ok || len(opt.Option) != 0 || opt.Version() != 0 {
			return "", false
		}
		dnssecOK = opt.Do()
		udpSize = opt.UDPSize()
	}
	checkingDisabled = request.CheckingDisabled
	question := request.Question[0]
	return fmt.Sprintf("%s|%s|%d|%d|%d|%t|%t|%t|%t", group, strings.ToLower(dns.Fqdn(question.Name)), question.Qtype, question.Qclass, udpSize, dnssecOK, checkingDisabled, request.RecursionDesired, request.AuthenticatedData), true
}

func responseTTL(message *dns.Msg, negativeMax, max uint32) (uint32, bool) {
	var soa *dns.SOA
	for _, record := range message.Ns {
		if candidate, ok := record.(*dns.SOA); ok {
			soa = candidate
			break
		}
	}
	negative := message.Rcode == dns.RcodeNameError || message.Rcode == dns.RcodeSuccess && len(message.Answer) == 0
	if negative {
		if soa == nil || soa.Hdr.Ttl == 0 || soa.Minttl == 0 {
			return 0, false
		}
		ttl := soa.Hdr.Ttl
		if soa.Minttl < ttl {
			ttl = soa.Minttl
		}
		if negativeMax < ttl {
			ttl = negativeMax
		}
		if max < ttl {
			ttl = max
		}
		if !capResponseRecords(message, max, &ttl) {
			return 0, false
		}
		soa.Hdr.Ttl = ttl
		return ttl, ttl > 0
	}
	var ttl uint32
	for _, section := range [][]dns.RR{message.Answer, message.Ns, message.Extra} {
		for _, record := range section {
			if _, ok := record.(*dns.OPT); ok {
				continue
			}
			recordTTL := record.Header().Ttl
			if recordTTL == 0 {
				return 0, false
			}
			if recordTTL > max {
				record.Header().Ttl = max
				recordTTL = max
			}
			if ttl == 0 || recordTTL < ttl {
				ttl = recordTTL
			}
		}
	}
	return ttl, ttl > 0
}

func capTTLs(message *dns.Msg, max uint32) {
	for _, section := range [][]dns.RR{message.Answer, message.Ns, message.Extra} {
		for _, record := range section {
			if _, ok := record.(*dns.OPT); ok {
				continue
			}
			if record.Header().Ttl > max {
				record.Header().Ttl = max
			}
		}
	}
}

func capResponseRecords(message *dns.Msg, max uint32, ttl *uint32) bool {
	for _, section := range [][]dns.RR{message.Answer, message.Ns, message.Extra} {
		for _, record := range section {
			if _, ok := record.(*dns.OPT); ok {
				continue
			}
			recordTTL := record.Header().Ttl
			if recordTTL == 0 {
				return false
			}
			if recordTTL > max {
				record.Header().Ttl = max
				recordTTL = max
			}
			if recordTTL < *ttl {
				*ttl = recordTTL
			}
		}
	}
	return true
}

func decrementTTLs(message *dns.Msg, seconds uint32) {
	for _, section := range [][]dns.RR{message.Answer, message.Ns, message.Extra} {
		for _, record := range section {
			if _, ok := record.(*dns.OPT); ok {
				continue
			}
			if record.Header().Ttl <= seconds {
				record.Header().Ttl = 0
			} else {
				record.Header().Ttl -= seconds
			}
		}
	}
}

func (c *responseCache) evictOldest() {
	var oldestKey string
	var oldestExpiration time.Time
	for key, entry := range c.entries {
		expires := entry.inserted.Add(entry.ttl)
		if oldestKey == "" || expires.Before(oldestExpiration) {
			oldestKey, oldestExpiration = key, expires
		}
	}
	if oldestKey != "" {
		delete(c.entries, oldestKey)
	}
}
