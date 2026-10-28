# Split DNS Helm Chart

This chart deploys a DNS forwarder with exact and label-boundary suffix routing. It supports ordinary DNS upstreams over UDP with TCP retry and RFC 8484 DNS-over-HTTPS upstreams over a direct or named HTTP CONNECT proxy egress path.

## Configuration

The chart requires an explicit upstream address. Its default uses the reserved documentation address `192.0.2.53:53`; it is intentionally non-routable and must be replaced before serving DNS queries. The chart does not guess a cluster DNS Service IP because service CIDRs differ between clusters.

Start with [examples/minimal.yaml](examples/minimal.yaml) for a single forward group, or [examples/routed.yaml](examples/routed.yaml) for a generic example that routes `example.org` through DoH and other names to an explicitly configured DNS server. The example IPs and domain are reserved documentation values; replace the upstream address before use.

Rules are case-insensitive, ignore a final root dot, match exact names or suffixes on label boundaries, and prefer the most specific domain. Exact matching wins over suffix matching for the same domain. Invalid references and duplicate normalized rules fail Helm rendering and startup validation.

DoH endpoints use strict TLS certificate and hostname verification. `bootstrapIPs` dials the supplied IP while retaining the endpoint hostname for TLS SNI, certificate checks, and HTTP authority. Proxy egress never falls back to a direct connection. Valid NXDOMAIN and NODATA are terminal answers; transport, HTTP, malformed-message, and SERVFAIL failures may retry endpoints within the group's overall query timeout.

Proxy credentials are read only from an existing Secret. For example:

```yaml
dns:
  proxies:
    egress-proxy:
      url: http://proxy.example.invalid:3128
      auth:
        existingSecret: proxy-auth
        usernameKey: username
        passwordKey: password
```

The Secret must exist in the release namespace. Credentials are injected as environment variables; they are not copied into the generated ConfigMap or proxy URL. Custom DoH CA bundles can reference a same-namespace Secret or ConfigMap with an optional key.

A DoH group selects this proxy with `egress.mode: proxy` and `egress.proxyRef: egress-proxy`. Replace the reserved `.invalid` proxy hostname with a reachable HTTP CONNECT proxy.

The DNS cache is disabled by default. If enabled, answers are capped only by shortening TTLs, each resource record keeps its own TTL, zero-TTL answers are not cached, negative caching requires an SOA and is bounded by its SOA values and `negativeMaxTTL`, and expired answers are not served stale.

Each upstream group has its own concurrency limit and query timeout. DNS and health listeners remain ready while an upstream is unavailable. Upstream failures return SERVFAIL; the service does not cross-fallback between groups. A TCP client disconnect cancels its in-flight upstream request; idle TCP connections close after 60 seconds. At most 16 queued queries are accepted per TCP connection. UDP has no cancellation message, so abandoned UDP queries remain bounded by the group's query timeout. Process shutdown cancels active upstream exchanges. Logs omit query names and credentials. Optional Prometheus metrics use bounded group, result, and cache labels without query-name labels.

## Install

Replace the documentation upstream address with a reachable DNS server, then install:

```sh
helm upgrade --install split-dns ./application/split-dns/chart \
  --namespace split-dns --create-namespace \
  -f ./application/split-dns/chart/examples/minimal.yaml
```

The Service is a ClusterIP with UDP/53 and TCP/53. If `service.clusterIP` is empty, Kubernetes allocates it on first install and Helm leaves it omitted on later upgrades. Read the allocated address with:

```sh
kubectl -n split-dns get service split-dns -o jsonpath='{.spec.clusterIP}'
```

The DNS pods use `ClusterFirst` for their own bootstrap and do not point to this Service. Configure an explicit, reachable upstream resolver address; do not use a loopback stub or this chart's own Service IP.

To direct a workload Pod to this DNS Service, set its Pod DNS policy and nameserver after recording the allocated Service IP:

```yaml
spec:
  template:
    spec:
      dnsPolicy: None
      dnsConfig:
        nameservers:
          - <split-dns-service-ip>
```

`dnsPolicy: None` makes the configured nameserver explicit for that Pod. Add search domains and resolver options only when the workload requires them. Do not add a second nameserver if every query must follow the selected split DNS policy.

## Upgrade and rollback

Run `helm upgrade` with the same release name, namespace, and values file. Config changes trigger a rolling Deployment update; the Service remains in place. The default update strategy uses zero unavailable replicas and one surge replica. With two replicas, the optional PDB defaults to `minAvailable: 1`.

To roll back a chart release:

```sh
helm history split-dns --namespace split-dns
helm rollback split-dns <revision> --namespace split-dns
```

If the DNS Service is reinstalled, its ClusterIP can change. Update any consumer's `dnsConfig.nameservers` to the new address before relying on it. Restore the consumer's previous DNS policy when removing the DNS Service.

## Troubleshooting and uninstall

Check local readiness separately from upstream behavior:

```sh
kubectl -n split-dns get pods,service,pdb
kubectl -n split-dns logs deployment/split-dns
kubectl -n split-dns port-forward deployment/split-dns 8080:8080
```

Enable `metrics.enabled` to expose `/metrics` on the configured Service port. Inspect `split_dns_requests_total`, `split_dns_upstream_attempts_total`, and `split_dns_cache_events_total` by group and result. Readiness checks only the local process; successful health checks do not prove upstream connectivity. Use `dig` over both UDP and TCP from an allowed client Pod and inspect proxy logs or a controlled upstream when verifying route selection.

Uninstall after removing consumer references:

```sh
helm uninstall split-dns --namespace split-dns
```
