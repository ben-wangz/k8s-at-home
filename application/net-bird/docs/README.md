# NetBird Documentation

This catalog documents the design of using NetBird to privately access a designated Kubernetes Service.

- [Service access plan](service-access.md): architecture, deployment steps, access model, security control, operation and maintenance process and acceptance criteria.
- [Resources and Relationships of Three Charts](chart-resources.md): Resource definition, ownership, coordination process and permission boundaries of three Charts.
- [Manually open a single Service](manual-test-service-access.md): Do not install the Operator, use a routing peer Deployment to open `basic-components/test-service:80`.

This plan specifically does not include access to the Kubernetes API, nodes, public Ingress, or the entire Pod CIDR or Service CIDR.
