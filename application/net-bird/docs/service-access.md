# NetBird private service access plan

## 1. Goals and Scope

Give developers and operations staff private access to a small set of explicitly approved Kubernetes `ClusterIP` Services from managed devices. Users connect through the NetBird client and reach the Service through the routing peer in the cluster. Services always remain private and do not require public Ingress, LoadBalancer, NodePort, or public DNS records.

This document is both a design proposal and an operations manual. It does not introduce a Helm chart or modify existing application deployments.

### In scope

- Self-hosted NetBird control plane.
- User devices that are OIDC authenticated and running the NetBird client.
- NetBird Kubernetes Operator.
- Highly available routing peer within the cluster.
- Explicitly approved `ClusterIP` Service and private DNS names.
- NetBird ACL, Kubernetes `NetworkPolicy`, application authorization and Service lifecycle operation and maintenance.

### Out of scope

- Kubernetes API access and `ClusterProxy`.
- Kubernetes node, VPC, local network subnet or SSH access.
- Access all Pods, all Services or the entire cluster CIDR.
- Provide public HTTP access via Ingress, Gateway API, or LoadBalancer.
- Inject NetBird sidecar into each Pod.

Within this scope, the Operator's `NetworkRouter` and `NetworkResource` modes are used. Gateway API integration is not part of the baseline because upstream marks it as beta.

## 2. Architectural Decisions

```text
                            Control plane traffic
                 OIDC, device registration, ACL, DNS, routing
                                  |
                                  v
                       +-----------------------+
                       | NetBird Control Plane |
                       | External VM or management cluster |
                       +-----------------------+
                                  ^
                                  |
Developer Workstation |
NetBird Client |
  |                               |
  | WireGuard, try to use P2P direct connection; use encrypted Relay when failure
  v
+-------------------------- Target Kubernetes cluster ----------------------------+
|                                                                         |
|  +------------------+          +------------------+                    |
| | Routing Peer Pod A | | Routing Peer Pod B | ... |
|  +------------------+          +------------------+                    |
|             \                         /                                |
|              +------- ClusterIP Service -------------------------------+
|                         grafana.observability.svc                      |
|                                                                         |
+-------------------------------------------------------------------------+
```

The NetBird control plane is deployed outside the target cluster to improve resiliency and operability. It is not a Kubernetes API gateway; it does not carry normal application traffic when peers can connect directly. The control plane is responsible for authenticating Peers, distributing public keys, routing, DNS configuration and access policies, and helping Peers negotiate connections.

The routing peer is located inside the target cluster and only forwards Service traffic declared through `NetworkResource`. This Service-only access scenario does not require an additional VPC gateway VM.

### Data plane boundaries

| Link | Protection method | Responsible component |
| --- | --- | --- |
| Workstation to Routing Peer | WireGuard encryption; traffic remains end-to-end encrypted even after passing through Relay | NetBird Client and Routing Peer |
| Routing Peer to `ClusterIP` Service | Kubernetes cluster networking | CNI, `NetworkPolicy` and application TLS/mTLS |
| Control Plane Connectivity | HTTPS, OIDC, Signal, and STUN/Relay Endpoints | NetBird Control Plane |

WireGuard encryption is terminated at the routing peer. If your Service payload requires true end-to-end encryption, you should use application layer TLS or mTLS and retain Kubernetes network controls to protect the last hop within the cluster.

## 3. NetBird components

| Components | Deployment Locations | Responsibilities |
| --- | --- | --- |
| Dashboard | Control plane | Interface for managing users, peers, groups, ACLs, DNS, routing, and activity records. |
| Management | Control plane | Peer registration, authentication, network topology, WireGuard public keys, policies, routing, and private DNS. |
| Signal | Control Plane | Exchange encrypted connection candidate information, not a data relay itself. |
| Relay and STUN | Control plane or standalone Relay host | Assists with NAT traversal; forwards encrypted traffic only if P2P direct connection fails. |
| Identity provider | Existing IdP or NetBird built-in IdP | User login and MFA. Production environments should use the organization's existing OIDC provider. |
| NetBird Client | Workstation and Routing Peers | Generate local WireGuard keys, apply routing, DNS and ACLs, and establish encrypted peer connections. |
| NetBird Kubernetes Operator | Target Cluster | Coordinates NetBird resources against Kubernetes CRs and creates routing peer workloads. |

NetBird's current self-hosted rapid deployment uses a combined `netbird-server` container to run Management, Signal, Relay and STUN, while deploying the Dashboard and reverse proxy. For detailed architecture, see [NetBird Architecture Document](https://docs.netbird.io/about-netbird/how-netbird-works).

## 4. Target access model

The examples throughout this document use the following values:

| Projects | Examples |
| --- | --- |
| NetBird control plane URL | `https://netbird.example.com` |
| Cluster name | `prod` |
| Private DNS Zone | `services.prod.example.internal` |
| Client Peer Group | `prod-service-clients` |
| Service resource group | `prod-service-backends` |
| Example Service | `grafana` in the `observability` namespace |
| Example Service Port | `3000/TCP` |
| Client visible name | `grafana.observability.services.prod.example.internal` |

NetBird access policy only allows:

```text
prod-service-clients -> prod-service-backends -> TCP/3000
```

Services with different owners, sensitivity levels, or ports should use independent target groups and policies. This scheme MUST NOT use the `All` group as a source or target.

## 5. Preconditions

### 5.1 Control plane

- A Linux VM separate from the target cluster, or a separate managed Kubernetes cluster.
- Public DNS name and valid TLS certificate for the control plane.
- Default self-hosted deployment requires access to TCP `80`, `443` and UDP `3478` from the public network.
- Location for backing up NetBird configurations, databases, and encryption keys.
- OIDC identity provider that provides MFA to production users.

The official quick deployment is suitable for proof of concept. It uses Docker Compose and can initialize local users. Production deployments should review the generated configuration, pin image and chart versions in GitOps, use external OIDC, and replace the default SQLite store with PostgreSQL. See [Self-hosted quick deployment](https://docs.netbird.io/selfhosted/selfhosted-quickstart) and [configuration reference](https://docs.netbird.io/selfhosted/maintenance/configuration-files).

### 5.2 Target cluster

- A Kubernetes cluster with at least two nodes; three nodes are recommended for a three-replica routing peer pool.
- Platform operators can use `kubectl` and Helm.
- `cert-manager` is installed, or has an equivalent webhook certificate scheme.
- Ability to create namespaces, Kubernetes Secrets, CRs, and `NetworkPolicy`.
- Prepare a dedicated NetBird Service User and Personal Access Token (PAT) for the Kubernetes Operator. Personal administrator tokens cannot be used.
- Approved rendered Operator `ClusterRole`. The current upstream chart, when configured using the `keyFromSecret` PAT, is granted cluster-wide Secret list/watch and write permissions. This must be an adoption gate: review the actual permissions of the pinned Chart version; if they are unacceptable, isolate the Operator in a dedicated cluster or complete a permissions redesign first.

### 5.3 Client Device

- Install the NetBird client for macOS, Windows, or Linux.
- Ability to access OIDC identity provider via browser.
- Ability to access the NetBird control plane outbound. Peer direct connection uses UDP when conditions permit; when direct connection is not possible, NetBird can use an encrypted Relay path.

## 6. Deployment process

### 6.1 Deploy and secure the control plane

1. Configure `netbird.example.com` outside the target cluster.
2. Follow the official guide to deploy self-hosted NetBird. Use the built-in reverse proxy only if it conforms to the existing TLS schema, otherwise configure the external reverse proxy integration as described in the documentation.
3. Create the first administrator, then connect to the organization's OIDC identity provider, and force MFA to be enabled before ordinary users can connect.
4. Save server configuration, database backups, encryption keys, and TLS materials outside of the VM. The recovery process must be verified before production access is granted.
5. Do not use the target Kubernetes cluster as the only deployment location for the control plane. In the event of a target cluster failure, the ability to manage access and investigate the incident should still be retained.

Open-source deployments can decouple Relay/STUN from the main service and migrate storage to PostgreSQL. Upstream currently lists active-active high availability for Management and Signal as an Enterprise capability. If "no single point of failure in the control plane" is a hard requirement that must be met by a purely open-source solution, document it as a NetBird product limitation before adoption. See [scaling self-hosted deployment](https://docs.netbird.io/selfhosted/maintenance/scaling/scaling-your-self-hosted-deployment) and [high availability](https://docs.netbird.io/selfhosted/maintenance/scaling/high-availability).

### 6.2 Create NetBird Groups, DNS and Policies

Before applying Kubernetes CRs, complete the following actions through the NetBird Dashboard or an API/Terraform workflow:

1. Create a Peer group named `prod-service-clients` and add only managed user devices that are allowed to access the production Service.
2. Create a resource group named `prod-service-backends`. It only represents the approved services in the cluster and does not represent all workloads in the cluster.
3. Create a custom DNS Zone `services.prod.example.internal` and distribute it only to `prod-service-clients`.
4. Create an ACL named `prod-grafana-access`, with source `prod-service-clients`, destination `prod-service-backends`, protocol `TCP`, and port `3000`.

NetBird Operator does not automatically create these user groups or ACLs. NetBird denies access by default, so being able to resolve DNS does not mean you have gained access to the traffic.

### 6.3 Install Kubernetes Operator

Create a Secret containing a dedicated NetBird Service User PAT. Never write the PAT to a source repository or Helm values. The following command creates a Secret in the expected format without putting the token directly in command-line arguments:

```shell
kubectl create namespace netbird
kubectl -n netbird create secret generic netbird-mgmt-api-key \
  --from-file=NB_API_KEY=/secure/path/netbird-operator-pat
```

Create and review `netbird-operator-values.yaml`. `managementURL` must be a self-hosted Management URL accessible from the target cluster.

```yaml
managementURL: https://netbird.example.com

cluster:
  name: prod
  dns: svc.cluster.local

netbirdAPI:
  keyFromSecret:
    name: netbird-mgmt-api-key
    key: NB_API_KEY

ingress:
  enabled: false

gatewayAPI:
  enabled: false
```

Install a pinned Operator version; do not rely on a mutable default version:

```shell
helm upgrade --install netbird-operator \
  oci://ghcr.io/netbirdio/helm-charts/netbird-operator \
  --namespace netbird \
  --create-namespace \
  --version <PINNED_OPERATOR_VERSION> \
  --values netbird-operator-values.yaml
```

Before creating a NetBird CR, confirm that the controller and webhooks are in place:

```shell
kubectl -n netbird get pods
kubectl get crd | rg 'netbird.io'
```

[NetBird Kubernetes Operator documentation](https://docs.netbird.io/use-cases/kubernetes) explains the installation process and PAT requirements. The current CR uses the `netbird.io/v1alpha1` API; pin the Operator version and test its CRD on a non-production cluster before upgrading.

### 6.4 Create a highly available Network Router

Create `NetworkRouter` in the `netbird` namespace. The referenced custom DNS zone must already exist.

```yaml
apiVersion: netbird.io/v1alpha1
kind: NetworkRouter
metadata:
  name: prod-services
  namespace: netbird
spec:
  dnsZoneRef:
    name: services.prod.example.internal
  workloadOverride:
    replicas: 3
    podTemplate:
      spec:
        topologySpreadConstraints:
          - maxSkew: 1
            topologyKey: kubernetes.io/hostname
            whenUnsatisfiable: DoNotSchedule
            labelSelector:
              matchLabels:
                app.kubernetes.io/name: networkrouter
                app.kubernetes.io/instance: prod-services
```

Apply resources and wait for routing peers to register:

```shell
kubectl apply -f network-router.yaml
kubectl -n netbird get pods -o wide
kubectl -n netbird get networkrouter prod-services -o yaml
```

Use three replicas only when the cluster has three independent, schedulable nodes; otherwise set the replica count to the number of available fault domains. Topology constraints aim for one Pod per node. Operator 0.3.1 does not generate a `PodDisruptionBudget`; if voluntary disruptions must be protected, create and audit a separate PDB. Newer Operator versions may generate PDBs automatically, so check the rendered and derived resources after every upgrade. The Operator's routing-peer mode supports failover between multiple peers.

### 6.5 Exposing an approved Service

The target must be a `ClusterIP` Service. Create one `NetworkResource` per approved Service; do not create a route for an entire namespace or CIDR.

```yaml
apiVersion: netbird.io/v1alpha1
kind: NetworkResource
metadata:
  name: grafana
  namespace: observability
spec:
  networkRouterRef:
    name: prod-services
    namespace: netbird
  serviceRef:
    name: grafana
  groups:
    - name: prod-service-backends
```

Apply resources and check status:

```shell
kubectl apply -f grafana-network-resource.yaml
kubectl -n observability get networkresource grafana -o yaml
kubectl -n observability get service grafana
```

The Operator creates DNS records in the following format:

```text
<service>.<namespace>.<zone>
```

In this example, the user accesses:

```text
grafana.observability.services.prod.example.internal
```

For related behaviors and high-availability routing Peer model, see [Route to Kubernetes Service through high availability](https://docs.netbird.io/use-cases/kubernetes/route-to-a-kubernetes-service).

### 6.6 Limit the last hop in the cluster

NetBird ACLs restrict overlay network access. Kubernetes `NetworkPolicy` should provide a second layer of control, but first confirm the source identity that the CNI presents to the target Pod. The Operator masquerades routed-peer traffic, so the workload may not see the original NetBird client overlay IP. Adjust labels and target ports to match the actual workload before applying the policy.

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: grafana-from-netbird
  namespace: observability
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: grafana
  policyTypes:
    - Ingress
  ingress:
    - from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: netbird
          podSelector:
            matchLabels:
              app.kubernetes.io/name: networkrouter
              app.kubernetes.io/instance: prod-services
      ports:
        - protocol: TCP
          port: 3000
```

This policy is just an example and assumes that CNI will recognize forwarded connections as traffic coming from the specified routing peer pod in the `netbird` namespace. This assumption must be verified in a pre-release environment; parts of the data path may present different source addresses after routing or SNAT. For matching Pods, this policy will replace the original unrestricted inbound status, so it should be merged with the application's existing necessary traffic rules and cannot be applied blindly.

## 7. Client access and daily use

### 7.1 User device registration

1. Install a supported NetBird client on the managed workstation.
2. Select the self-hosted deployment in the desktop client and enter `https://netbird.example.com`.
3. Log in via OIDC and MFA.
4. Confirm that the Peer has appeared in the Dashboard and added `prod-service-clients`.
5. Before trying to access the Service, confirm that the access policy has taken effect.

Linux CLI users can register and check connections using the following command:

```shell
netbird up --management-url https://netbird.example.com
netbird status -d
```

For unattended infrastructure peers, a Setup Key should be used that is single-use, has an expiration time, and is stored in the platform key manager. Setup Key cannot be used for normal interactive user registration.

### 7.2 Access the Service

After the client connection is successful and the ACL has been distributed:

```shell
getent hosts grafana.observability.services.prod.example.internal
curl --fail --silent --show-error \
  http://grafana.observability.services.prod.example.internal:3000/
```

`https://` and the Service's TLS port can only be used if the application itself terminates TLS; NetBird Overlay does not automatically convert HTTP Services to HTTPS.

DNS resolution and TCP connections must fail when the peer is not a member of `prod-service-clients`, when the requested port is not covered by the ACL, or when the associated NetBird policy has been removed.

Users still need to use the application's original credentials to access the Service. NetBird only provides network reachability and is not a replacement for application layer authentication and authorization.

## 8. Security requirements

### 8.1 Required Controls

- Human users must use OIDC MFA.
- Separate NetBird Peer groups by environment and access level.
- The policy must explicitly specify the source, destination, protocol, and port.
- Only explicitly named `ClusterIP` Services can be exposed through `NetworkResource`.
- Preserve application authentication and use TLS/mTLS as needed.
- Apply Kubernetes `NetworkPolicy` for the target workload.
- Before implementing `NetworkPolicy`, verify the identity of the routing peer source that CNI actually sees.
- Operator PAT can only be stored in a Kubernetes Secret or external key store.
- Operators must use dedicated Service Users and cannot reuse employee privilege tokens.
- Operator RBAC rendered by fixed chart versions must be reviewed and explicitly approved before installation.
- Pin Operator and NetBird control-plane versions and upgrade them through a proven pre-release process.
- Regularly audit NetBird Peer, group, policy and activity records.

### 8.2 Explicitly Prohibited

- Pod CIDR or Service CIDR must not be distributed as a generic NetBird route.
- Policies `All` to `All` must not be created.
- A public Ingress must not be used as an alternative to accessing a Service.
- The only NetBird control plane must not be deployed inside the target cluster.
- PAT, Setup Key, private keys, or generated kubeconfig files must not be submitted.
- It should not be assumed that revoking a Setup Key will disconnect peers already registered with that key. When a person logs out or the device goes offline, the peer or access policy should be deleted.

For the Setup Key life cycle, see [Registering Machines with Setup Key](https://docs.netbird.io/manage/peers/register-machines-using-setup-keys).

## 9. Operation, maintenance and troubleshooting

| Events | Expected Behavior | Operations |
| --- | --- | --- |
| A routing Peer Pod fails | When multiple ready peers exist, the client can switch to other registered peers | Check the number of replicas, Pod distribution, separately maintained PDB and routing Peer health status |
| One node fails | If there are still routing peers on other nodes and the Service has a healthy endpoint, access continues to be available | Check the topological distribution of applications and routing peers |
| Target Service has no Endpoint | NetBird connection is OK, but application request fails | Troubleshooting Service, Endpoint, Pod and `NetworkPolicy` |
| Control plane failure | Existing data plane connections may continue to work, but login, registration, policy updates, and failure recovery are impaired | Recover the control plane using monitored infrastructure and backups |
| User device lost | Device may retain access until peer is deleted | Delete peer immediately, remove group membership, and revoke IdP access |
| Operator PAT rotation | Operator coordination fails before Secret update | Rotate Secret, restart or trigger Operator coordination, and check CR status |

### 9.1 Daily inspection

```shell
kubectl -n netbird get pods -o wide
kubectl -n netbird get networkrouter
kubectl -A get networkresource
kubectl -n netbird logs deploy/netbird-operator
```

On the user device:

```shell
netbird status -d
getent hosts grafana.observability.services.prod.example.internal
```

### 9.2 Backup and recovery

Back up the following and verify the recovery process:

- NetBird database and its encryption keys.
- NetBird control plane configuration and reverse proxy configuration.
- Status of TLS certificates when not managed by an external system.
- Declarative Kubernetes configuration for `NetworkRouter`, `NetworkResource`, `NetworkPolicy` and Operator values.
- Key management system that provides data for Operator PAT.

Do not back up the user device's WireGuard private key as a shared infrastructure secret. A new peer should be registered when changing devices.

## 10. Acceptance Criteria

Implementation is considered complete only if all of the following conditions are met:

- The target Service type is `ClusterIP` and has no public exposure method.
- Only approved managed devices can resolve private service names.
- Only approved client groups can connect to approved Service ports.
- Clients that do not belong to this group cannot resolve or connect to the Service.
- The target Pod only accepts traffic that follows the expected Kubernetes policy path.
- At least two routing peers are healthy; use three peers when the cluster has three independent nodes.
- Deleting a routed peer pod will not cause ongoing client outage.
- After a user's NetBird Peer is deleted, the user can no longer access the Service.
- A NetBird control-plane backup is successfully restored in a test environment.
- Rendered Operator RBAC reviewed and approved.
- No PAT, Setup Key or other credentials exist in Git and Helm values.

## 11. References

- [NetBird Architecture](https://docs.netbird.io/about-netbird/how-netbird-works)
- [Self-hosted quick deployment](https://docs.netbird.io/selfhosted/selfhosted-quickstart)
- [Self-hosted configuration files](https://docs.netbird.io/selfhosted/maintenance/configuration-files)
- [NetBird Kubernetes Operator](https://docs.netbird.io/use-cases/kubernetes)
- [Routing Peer Reference](https://docs.netbird.io/use-cases/kubernetes/routing-peer)
- [Route to a Kubernetes Service](https://docs.netbird.io/use-cases/kubernetes/route-to-a-kubernetes-service)
- [Network Access Policy](https://docs.netbird.io/manage/access-control/manage-network-access)
- [Setup Key Life Cycle](https://docs.netbird.io/manage/peers/register-machines-using-setup-keys)
