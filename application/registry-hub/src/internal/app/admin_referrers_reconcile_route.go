package app

import (
	"registry-hub/internal/route"
	"strings"
)

func reconcileRoute(repository, subject string) (route.Route, error) {
	if !route.ValidDigest(subject) || repository == "" || strings.ContainsAny(repository, "?#\\\r\n\x00") {
		return route.Route{}, route.ErrInvalidPath
	}
	authority, remote, found := strings.Cut(repository, "/")
	if !found {
		internal, err := route.InternalName(repository)
		if err != nil {
			return route.Route{}, err
		}
		return route.Route{Repository: repository, Internal: internal, Reference: subject, Endpoint: route.EndpointReferrers}, nil
	}
	if route.ValidateAuthority(authority) != nil || route.ValidateRepository(remote) != nil {
		return route.Route{}, route.ErrInvalidPath
	}
	internal, err := route.InternalName(repository)
	if err != nil {
		return route.Route{}, err
	}
	return route.Route{Repository: repository, Internal: internal, Reference: subject, Endpoint: route.EndpointReferrers, Upstream: authority, RemoteRepo: remote, HasUpstream: true}, nil
}
