# NetBird: Resources and Relationships of Three Helm Charts

This document starts with the basic concepts of Kubernetes resources, explains what each of the three charts creates, identifies who uses those resources, and describes what happens after a Service is declared accessible.

The three charts discussed in this document are:

1. `netbird`: NetBird self-hosted control plane chart.
2. `kubernetes-operator`: NetBird Kubernetes Operator Chart.
3. `netbird-service-access`: Service access statement chart we plan to maintain.

The third chart is not an official NetBird chart. It is only responsible for submitting access intentions and does not run the controller.

## Version boundaries

The resource list is based on the official chart snapshots selected for this solution: `netbird` chart 1.9.0 and `kubernetes-operator` chart 0.3.1. The Operator image, CRDs, and Helm chart must use matching versions; when upgrading, re-render and review the resources and RBAC.

Later versions of the NetBird Operator add new APIs and derived resources. These changes are covered in the "Version differences" section and are not mixed into the 0.3.1 baseline list.

This solution only implements:

- Access the specified Kubernetes `ClusterIP` Service from the NetBird client.
- Use `NetworkRouter` and `NetworkResource`.
- Use existing user groups, resource groups, DNS zones, and access policies from the NetBird control plane.

This plan does not enable:

- Kubernetes API proxy or `ClusterProxy`.
- Overall routing for Node, VPC, Pod CIDR, or Service CIDR.
- Old `ingress` mode for Operator.
- Gateway API integration.
- NetBird sidecar injection.

## 1. First understand a few basic concepts

### 1.1 Helm Chart, Kubernetes Resources and CRDs

| Name | Meaning | Examples in this scenario |
| --- | --- | --- |
| Helm Chart | A set of templated Kubernetes YAML responsible for installing and upgrading resources | `netbird`, `kubernetes-operator` |
| Kubernetes resource | An object in the Kubernetes API | `Deployment`, `Service`, `Secret` |
| CRD | Register a new resource type with Kubernetes | `networkrouters.netbird.io` |
| CR | An instance created from a CRD | `kind: NetworkRouter` |
| Controller | Continuously observe resources and adjust the actual state to the state described by `spec` | NetBird Operator |
| Remote NetBird object | Exists in NetBird Management, not in Kubernetes API | Network, Router, Group, DNS Record |

A CRD is only a "type definition" and does not represent an actual router. Kubernetes only recognizes the `NetworkRouter` type after installing the Operator Chart; then the third Chart creates a specific `NetworkRouter` object.

### 1.2 `spec`, `status` and OwnerReference

A typical Kubernetes object can be abstracted as:

```yaml
apiVersion: netbird.io/v1alpha1
kind: NetworkResource
metadata:
  name: grafana
  namespace: observability
spec: {}
```

The above `spec` is submitted by the user or Helm; `status` is not submitted by the user, but is written back by the Operator.

- `metadata` identifies the object's name, namespace, label and deletion policy.
- `spec` is the desired state, usually managed by GitOps or Helm.
- `status` is what the controller observes and should not be maintained manually in Git.
- `OwnerReference` represents an affiliation. For example, the `SetupKey` created by the Operator is owned by the `NetworkRouter`; Kubernetes can garbage collect it when the parent object is deleted.
- `finalizer` allows the controller to delete the corresponding remote NetBird object before deleting the Kubernetes object.

### 1.3 Three different control chains

```text
Helm/GitOps
    | Render and submit Kubernetes objects
    v
Kubernetes API
    | Save CRD, CR and native resources
    v
NetBird Operator
    | Read CR and create Kubernetes derived resources
    | Call Management API
    v
NetBird Management
    | Save remote objects such as Network, Router, Group, DNS, ACL, etc.
    v
NetBird client obtains routing, DNS, and access policies
```

Ordinary business traffic is not equal to control traffic. Clients usually connect directly to routing peers through WireGuard; Management, Signal, and Relay are responsible for authentication, key exchange, connection negotiation, and relaying when necessary.

## 2. Overall division of labor among the three Charts

| Chart | Deployment location | Main responsibilities | Whether to run the controller | Whether to create business traffic Pods |
| --- | --- | --- | --- | --- |
| `netbird` | Manage cluster or manage namespace | Provides NetBird Management, Signal, Relay, Dashboard | No | No |
| `kubernetes-operator` | Target business cluster | Register NetBird CRD, listen to CR, and create routing peer | Yes | Yes, create routing peer Deployment |
| `netbird-service-access` | Target business cluster | Declare which Services are exposed to NetBird | No | No |

The recommended deployment boundaries are:

```text
Manage cluster
└── netbird Chart
    ├── Management
    ├── Signal
    ├── Relay
    └── Dashboard

Target business cluster
├── kubernetes-operator Chart
│   └── Operator, CRD, Webhook, RBAC
└── netbird-service-access Chart
    ├── NetworkRouter
    ├── NetworkResource
    └── Kubernetes NetworkPolicy
```

`netbird` Chart and Operator communicate with each other through Management URL and PAT/API Key, and do not call each other through Kubernetes ServiceAccount.

## 3. `netbird` Chart: Control plane resources

Official Chart reference: [netbird Helm Chart](https://github.com/netbirdio/helms/tree/main/charts/netbird).

### 3.1 Resource structure

The namespace is usually pre-created by `helm --create-namespace` or by the platform infrastructure; it is not a core template resource for this Chart.

```text
netbird namespace
├── Deployment netbird-management
│   ├── ConfigMap netbird-management
│   ├── PersistentVolumeClaim netbird-management
│   ├── Service netbird-management
│ ├── Service netbird-management-grpc [Optional compatible service]
│ └── Ingress [optional]
├── Deployment netbird-signal
│   ├── Service netbird-signal
│ └── Ingress [optional]
├── Deployment netbird-relay
│   ├── Service netbird-relay
│ └── Ingress [optional]
├── Deployment netbird-dashboard
│   ├── Service netbird-dashboard
│ └── Ingress [optional]
├── ServiceAccount × 4
└── ServiceMonitor [optional]
```

Deployment will further create ReplicaSet and Pod by Kubernetes. ReplicaSet and Pod are not top-level resources directly defined in the Helm template, but they are the resources that actually run the container.

### 3.2 Component resource definition

| Resource | Quantity/conditions | Role | Dependencies |
| --- | --- | --- | --- |
| `Deployment` Management | 1, `management.enabled=true` | Server side for NetBird control plane API, authentication, network and policy status | Mount Management ConfigMap and persistent volumes; exposed by Management Service |
| `Service` Management | 1 | Provide management with a stable address and port in the cluster | selector points to the Management Pod; Ingress can forward to it |
| `Service` Management gRPC | Optional | Compatible with older gRPC exposure methods | Created only if `useBackwardsGrpcService=true` |
| `ConfigMap` Management | 1 | Save `management.json` configuration | Mount to `/etc/netbird` of Management Pod as a file |
| `PersistentVolumeClaim` Management | Created by default | Save the persistent state of Management | Mount to `/var/lib/netbird` of Management Pod; use `emptyDir` after closing |
| `ServiceAccount` Management | 1 | Provide a Kubernetes identity for the Management Pod | The current Chart does not have a `Role` or `ClusterRole` bound to it |
| `Deployment` Signal | 1, `signal.enabled=true` | Assists clients in exchanging connection candidate information | Exposed by Signal Service and optional Ingress |
| `Service` Signal | 1 | Provide a stable cluster address for Signal | selector points to Signal Pod |
| `ServiceAccount` Signal | 1 | Provides a Kubernetes identity for the Signal Pod | Currently no additional Kubernetes RBAC |
| `Deployment` Relay | 1, `relay.enabled=true` | Relays encrypted traffic when the client cannot connect directly | Exposed by Relay Service and optional Ingress |
| `Service` Relay | 1 | Provide stable address and port for Relay | selector points to Relay Pod |
| `ServiceAccount` Relay | 1 | Provides a Kubernetes identity for the Relay Pod | Currently no additional Kubernetes RBAC |
| `Deployment` Dashboard | 1, `dashboard.enabled=true` | Web management interface | Exposed by Dashboard Service and optional Ingress |
| `Service` Dashboard | 1 | Provide a stable address in the cluster for Dashboard | selector points to Dashboard Pod |
| `ServiceAccount` Dashboard | 1 | Provides a Kubernetes identity for Dashboard Pods | Currently no additional Kubernetes RBAC |
| `Ingress` | Each component is created on demand | Forward external HTTP/gRPC requests to the corresponding Service | The dependent cluster already has an Ingress Controller; TLS Secret is usually provided by an external certificate process |
| `ServiceMonitor` | Optional | Let Prometheus Operator crawl metrics | Depend on Prometheus Operator's CRD |
| `extraManifests` | Optional | Render additional objects provided by the user | Do not belong to Chart fixed resources and must be reviewed separately |

### 3.3 Relationship between four workloads

- Management is the core of the control plane, saving device, user, network, routing and policy-related status.
- Signal only assists in connection negotiation and is not a general forwarding point for application traffic.
- Relay forwards still-encrypted traffic when a P2P connection fails.
- Dashboard is the management interface and usually works through the Management API.
- Both the client and the routing peer in the target cluster are registered through Management and obtain the network topology, DNS and ACL from Management.

### 3.4 What does this Chart not contain?

This Chart does not automatically create the following resources or services:

- PostgreSQL or other external database.
- OIDC Identity Provider.
- Ingress Controller, LoadBalancer or DNS Controller.
- Business credentials such as PAT, OIDC key, database password in `Secret`.
- Kubernetes `Role`, `ClusterRole`, `RoleBinding` or `ClusterRoleBinding`.
- NetBird Kubernetes CRD.

Chart's `envFromSecret` and `envRaw` simply reference or inject existing Secrets; they do not securely generate credentials for the user.

### 3.5 Permission boundaries of Chart 1

Helm/GitOps installation identities for control plane charts typically only require permissions within the `netbird` namespace:

- `apps/deployments`.
- `services`, `serviceaccounts`, `configmaps`, `persistentvolumeclaims`.
- `networking.k8s.io/ingresses` when Ingress is enabled.
- `monitoring.coreos.com/servicemonitors` when monitoring is enabled.
- When Helm uses Secret to store releases, Helm management permissions for the namespace Secret.

The ServiceAccounts for the four workloads do not have cluster-level Kubernetes API permissions by default. They require access to a database, OIDC, or external network, but that's not Kubernetes RBAC.

## 4. `kubernetes-operator` Chart: CRD and controller

Official Operator reference: [NetBird Kubernetes Operator](https://github.com/netbirdio/kubernetes-operator).

### 4.1 Chart directly installed resources

```text
cluster level
├── CustomResourceDefinition × 10
├── ClusterRole
├── ClusterRoleBinding
├── MutatingWebhookConfiguration
└── ValidatingWebhookConfiguration [created only in old ingress mode]

Operator namespace
├── ServiceAccount
├── Deployment
├── Service webhook
├── Service metrics [enabled by default]
├── Role
├── RoleBinding
├── Certificate + Issuer [cert-manager is used by default]
│ or Secret [when cert-manager is not used]
├── GatewayClass × 2                       [gatewayAPI.enabled=true]
├── NBPolicy [when configuring ingress.policies]
└── NBRoutingPeer [in old ingress mode]
```

CRD, ClusterRole, ClusterRoleBinding, WebhookConfiguration, and GatewayClass are cluster-level resources; they cannot be installed through a normal `Role` that only allows the business namespace.

### 4.2 Operator’s core basic resources

| Resources | Function | Does this solution depend on the data path |
| --- | --- | --- |
| `Deployment` Operator | Runs the controller and admission webhook server | Yes; the actual coordinator of all CRs |
| `ServiceAccount` Operator | The identity to access the Kubernetes API as an Operator | Yes; associated with the ClusterRoleBinding |
| `ClusterRole` | Defines the Operator's permissions to observe and modify resources across namespaces | Yes; the current upstream permissions scope is wider |
| `ClusterRoleBinding` | Binds the ClusterRole to the Operator ServiceAccount | Yes |
| `Role` | Provides leader election, ConfigMap and Event permissions in the Operator namespace | Yes |
| `RoleBinding` | Bind the above Role to Operator ServiceAccount | Yes |
| webhook `Service` | Allows the API Server to access the Operator's HTTPS webhook | Required during installation; not a business traffic entrance |
| metrics `Service` | Expose Operator metrics | Only affects monitoring, not routing |
| `MutatingWebhookConfiguration` | Calls Operator when Pod is created, supports SidecarProfile or old injection logic | Baseline does not use sidecar, but Chart is still installed by default |
| `ValidatingWebhookConfiguration` | Validating old `NBGroup` removal behavior | Do not install when baseline `ingress.enabled=false` |
| `Certificate`, `Issuer` | Generate TLS certificate for webhook Service | Default `enableCertManager=true` depends on cert-manager |
| webhook `Secret` | Save webhook TLS material when not using cert-manager | Alternative to Certificate/Issuer |
| `GatewayClass` | Register the NetBird Gateway API controller | Disabled in this solution |

Webhook's `namespaceSelector` only limits which objects the admission webhook receives and cannot reduce the observation scope of the Operator's own `ClusterRole`.

Operator Chart is not responsible for generating Management PAT. It just references an existing Secret:

```yaml
netbirdAPI:
  keyFromSecret:
    name: netbird-mgmt-api-key
    key: NB_API_KEY
```

Secrets should be created by External Secrets, a key management system, or a controlled bootstrap process; PATs should not appear in Git or normal Helm values.

### 4.3 CRD installed by Operator Chart

The currently selected chart snapshot contains 10 CRDs. They are divided into new APIs, Operator internal collaboration APIs, and compatible APIs.

#### CRD used in this solution

| CRD | Scope | Main fields | Function |
| --- | --- | --- | --- |
| `NetworkRouter` | Namespaced | `spec.dnsZoneRef`, `spec.workloadOverride` | Declare a shared NetBird network and routing peer pool |
| `NetworkResource` | Namespaced | `spec.networkRouterRef`, `spec.serviceRef`, `spec.groups` | Map a ClusterIP Service in the current namespace to the specified NetworkRouter |
| `Group` | Namespaced | `spec.name` | Represents a NetBird Group managed by the Operator; usually automatically created by NetworkRouter |
| `SetupKey` | Namespaced | `spec.name`, `ephemeral`, `autoGroups` | Represents a NetBird Setup Key managed by the Operator; usually automatically created by NetworkRouter |

#### CRDs installed but not used by the baseline

| CRD | Function | Why not use |
| --- | --- | --- |
| `SidecarProfile` | Inject the NetBird client sidecar according to the Pod selector | This solution uses an independent routing peer and does not inject the NetBird client into the business Pod |
| `NBGroup` | Legacy v1 Group API | Legacy compatibility model |
| `NBSetupKey` | Old v1 Setup Key API | Old compatibility model |
| `NBResource` | Old v1 resource API | Old Service annotation/ingress path |
| `NBRoutingPeer` | Old v1 routing peer API | Old ingress path |
| `NBPolicy` | Old v1 NetBird Access Policy API | This solution manages ACLs separately in the NetBird control plane and is not automatically generated by the Chart |

`NBPolicy` is not the same resource as Kubernetes `NetworkPolicy`: the former controls NetBird overlay access, and the latter controls Kubernetes Pod network traffic.

### 4.4 Definition and derived resources of `NetworkRouter`

`NetworkRouter` is the desired state submitted by the user. It is not a Pod, nor a Kubernetes Service; it is the entry point used by the Operator to generate a set of remote and local resources.

Minimal example:

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
```

Field meaning:

- `dnsZoneRef.name` must refer to an existing DNS Zone in NetBird Management.
- `workloadOverride.replicas` controls the number of routing peer Pods; the default value for Operator 0.3.1 is 3.
- `workloadOverride.podTemplate` can override the Pod template, but any changes to privileges, mounting, and node scheduling need to be reviewed separately.
- `status.networkID` and `status.routingPeerID` are remote object IDs, written back by the Operator.

In Operator 0.3.1, a `NetworkRouter` would have the following relationships:

```text
NetworkRouter/prod-services
├── Group/networkrouter-prod-services
│   └── NetBird Group
├── SetupKey/networkrouter-prod-services
│   ├── NetBird Setup Key
│   └── Secret/setup-key-networkrouter-prod-services
├── NetBird Network
├── NetBird Router
└── Deployment/networkrouter-prod-services
    ├── ReplicaSet
    └── routing peer Pods
```

Specific functions:

1. The Operator first checks whether the DNS Zone exists and creates or updates the Network in NetBird.
2. Operator creates a `Group` CR; Group Controller creates the corresponding Group in NetBird and writes the remote ID into `status.groupID`.
3. The Operator creates a `SetupKey` CR; the SetupKey Controller creates a temporary Setup Key in NetBird and generates a Kubernetes Secret. The data key of Secret is `setup-key`.
4. Operator creates a Router in the Network and adds it to the remote Group above. The current implementation sets Router to `Masquerade=true`.
5. Operator creates routing peer Deployment. The Pod uses the Setup Key in the Secret to register with NetBird.
6. Deployment creates ReplicaSet and Pod. The routing peer Pod is responsible for forwarding overlay traffic to the Service address in the cluster.

Operator 0.3.1 directly creates Deployment and does not directly create routing peer Service, PDB, Role or RoleBinding. Subsequent Operator versions may add these derived resources, and manifests from different versions cannot be mixed.

The routing peer container needs to modify the network stack. The Pod generated by Operator 0.3.1 uses `privileged` and `NET_ADMIN`, `SYS_ADMIN`, and `SYS_RESOURCE` capabilities. This is a container/pod security permission, not Kubernetes RBAC; the target cluster's Pod Security Admission must allow it.

### 4.5 Definition and derived objects of `NetworkResource`

`NetworkResource` is a declaration that "exposes a Service". It does not create a Service itself, nor a Deployment.

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

Field meaning:

- `networkRouterRef` is a cross-namespace reference pointing to a shared `NetworkRouter`.
- `serviceRef` is a Service name reference within the current namespace.
- `groups` is a NetBird remote resource group reference that determines which target groups the resource belongs to.

The Operator performs the following checks and operations:

1. Find the `observability/grafana` Service.
2. Requires the Service type to be `ClusterIP` and have a non-empty ClusterIP; `ExternalName`, `NodePort` or headless Service is not accepted.
3. Read the `status.networkID` of `NetworkRouter` and the router DNS Zone.
4. Create or update a remote Network Resource in the NetBird Network, and the address uses the ClusterIP of the Service.
5. Create a record in the referenced DNS Zone:

   ```text
   <service>.<namespace>.<zone>
   ```

6. Write the remote Network ID, Resource ID, DNS Zone ID and DNS Record ID to `NetworkResource.status`.
7. When deleting `NetworkResource`, delete the remote Resource and DNS Record through the finalizer.

Therefore, the relationship of `NetworkResource` is:

```text
NetworkResource
├── Quote Kubernetes Service
├── Reference NetworkRouter across namespaces
├── Read Service.ClusterIP
├── Write to NetBird Network Resource
└── Write to NetBird DNS Record
```

`Ready=True` of `NetworkResource` only means that the Operator has completed remote object synchronization. It does not mean that the application Pod is necessarily healthy, nor does it mean that NetBird ACL has allowed a certain user to access it.

### 4.6 Things that Operator won’t do automatically

In this scenario, the following objects need to be managed in the NetBird control plane in advance or separately:

- User and device registration.
- Client group, such as `prod-service-clients`.
- Resource group, such as `prod-service-backends`.
- DNS Zone.
- ACL from the client group to the resource group, specifying the protocol and port.

Adding a resource to `groups` only groups it in NetBird; it is not itself an ACL that allows traffic.

## 5. `netbird-service-access` Chart: Access declared resources

This is the third chart planned for this repository. It contains no Deployment, Pod, or controller, so it has no runtime; it only commits objects when Helm/GitOps applies it.

### 5.1 Resource structure

It is recommended to use two release forms of the same Chart to avoid multiple business applications competing for the same Router:

```text
Platform release: netbird-router
└── NetworkRouter, one for each cluster/environment

Business release: netbird-service-access
├── NetworkResource, a Service that allows access
└── NetworkPolicy, one for each target workload, optional but recommended
```

A platform release can also render Router and multiple Service resources at the same time, but it must be ensured that Router has only one owner.

### 5.2 Chart resource definition

| Resource | API group | Function | Creates a running workload |
| --- | --- | --- | --- |
| `NetworkRouter` | `netbird.io/v1alpha1` | Declare the number of shared NetBird networks, DNS Zones, and routing peers | No; derived from Operator Deployment |
| `NetworkResource` | `netbird.io/v1alpha1` | Declare that an existing Service is exposed to the NetBird network | No; the Operator calls the Management API |
| `NetworkPolicy` | `networking.k8s.io/v1` | Restrict the target Pod to only accept traffic from the routing peer | No; policy is enforced by CNI |

The third Chart should not be created:

- `Secret`, especially PAT, Setup Key or database credentials.
- `ServiceAccount`, `Role`, `ClusterRole`.
- `Deployment`, `Service`, `Ingress`.
- Operator CRD.
- NetBird `NBPolicy` or remote ACL.

It only references the Service that the business team has created. Service's Deployment, Pod, Endpoint and application authentication are still managed by the business chart.

### 5.3 Location of Kubernetes `NetworkPolicy`

NetBird ACL and Kubernetes `NetworkPolicy` are two different controls:

```text
client
  -- NetBird ACL --> routing peer
  -- Kubernetes NetworkPolicy --> Target Pod
  -- Service/Endpoint --> Application Pod
```

`NetworkPolicy` does not create NetBird routes or allow clients to automatically gain access. It only restricts intra-cluster jumps from the routing peer to the target Pod; the actual source Pod label and SNAT behavior must be verified against the CNI in the test cluster.

Indicative resources:

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

This is an example of a template and should not be applied directly. Existing ingress rules such as Prometheus, Ingress, and inter-service calls must be merged and retained; otherwise, the new policy may block the original traffic.

## 6. Complete relationship from declaration to access

Taking `observability/grafana` as an example, the complete link is as follows:

```text
1. netbird Chart
   Management Service
        ^
        | HTTPS + PAT/API Key
        |
2. kubernetes-operator Chart
   Operator Deployment
        ^
        | watches
        |
3. netbird-service-access Chart
   NetworkRouter/prod-services (netbird namespace)
   NetworkResource/grafana (observability namespace)
   NetworkPolicy/grafana-from-netbird
        |
        +--> NetworkRouter
        |      +--> NetBird Network + Router
        |      +--> Group + SetupKey + Secret
        |      +--> routing peer Deployment
        |
        +--> NetworkResource
               +--> Read Service/grafana.ClusterIP
               +--> NetBird Resource + DNS Record

NetBird client
    | Get ACLs, DNS and routes
    v
routing peer Pod
    | Visit ServiceClusterIP
    v
Service/grafana
    | Find Endpoints based on selector
    v
grafana application Pods
```

Here are three sets of names not to be confused with:

- Kubernetes `Service`: the virtual service address within the cluster.
- NetBird `NetworkResource`: The mapping of this Service ClusterIP in the control plane.
- NetBird DNS Record: Private name used by the client, for example `grafana.observability.services.prod.example.internal`.

## 7. Scope of authority

### 7.1 The installation identity and running identity must be separated

| Identity | Role | Recommended scope |
| --- | --- | --- |
| Control Plane Helm/GitOps Identity | Install `netbird` Chart | Manage only the cluster's `netbird` namespace resources |
| Operator bootstrap identity | Install/upgrade CRDs, ClusterRole, and Webhooks | Controlled cluster-wide published identity; not published as a daily application |
| Operator ServiceAccount | Runtime coordination of CR and derived resources | Upstream 0.3.1 default binding cluster-level ClusterRole |
| Service-access GitOps Identity | Commit Router, Resource, NetworkPolicy | Grant only `netbird` and approved business namespaces |
| Normal NetBird user | Access allowed Services | No Kubernetes RBAC is granted, and no PAT is issued |

### 7.2 `netbird` Chart installation permissions

Manage only the resources in the control-plane namespace:

- `Deployment`.
- `Service`, `ServiceAccount`, `ConfigMap`, `PersistentVolumeClaim`.
- Optional `Ingress` and `ServiceMonitor`.
- Helm release stores the required Secrets.

If the namespace does not exist, creating the namespace is an additional permission and is not a permission of the Chart workload ServiceAccount.

### 7.3 Operator Chart installation permissions

The bootstrap identity needs to be able to create or update at least:

- `CustomResourceDefinition`.
- `ClusterRole`, `ClusterRoleBinding`.
- `MutatingWebhookConfiguration`, optional `ValidatingWebhookConfiguration`.
- `ServiceAccount`, `Deployment`, `Service`, `Role`, `RoleBinding` in the Operator namespace.
- `Certificate` and `Issuer` when cert-manager is enabled, or webhook TLS `Secret` in non-cert-manager mode.
- `GatewayClass` when Gateway API is enabled.

CRDs are cluster-level objects and cannot be installed via a Role bound only to the `netbird` namespace.

### 7.4 Operator running permissions

The upstream `ClusterRole` of Operator 0.3.1 roughly includes the following scope:

| API Resources | Typical Operations |
| --- | --- |
| CR under `netbird.io` and its `status`, `finalizers` | get/list/watch/create/update/patch/delete, distinguished by sub-resources |
| `services` and `services/finalizers` | get/list/watch/update/patch |
| `namespaces` | get/list/watch |
| `apps/deployments` | get/list/watch/create/update/patch/delete |
| `pods` | get/list/watch |
| `secrets` | When using `keyFromSecret`, get/list/watch and create/update/patch/delete across namespaces |
| `configmaps`, `leases`, `events` | leader election and event permissions within the Operator namespace |
| Gateway API resources | Only increased when `gatewayAPI.enabled=true` |

This means that when using the regular `netbirdAPI.keyFromSecret` configuration, the Operator is not running as "read only one PAT Secret". The upstream template will give it cluster-wide Secret read and write permissions, which are permission boundaries that must be explicitly accepted or modified before the Chart can be adopted.

In addition, routing peer Pod's `privileged`, `NET_ADMIN`, etc. are Linux container security permissions and have nothing to do with the Kubernetes API permissions in the above table. The cluster must also allow the Pod to pass the Pod Security Admission/CNI security policy.

### 7.5 `netbird-service-access` GitOps permissions

The third Chart does not require a runtime ServiceAccount. The GitOps identity responsible for apply can be split by namespace:

In the `netbird` namespace:

- get/list/watch/create/update/patch/delete for `netbird.io/networkrouters`.

In each approved business namespace:

- get/list/watch/create/update/patch/delete for `netbird.io/networkresources`.
- get/list/watch/create/update/patch/delete for `networking.k8s.io/networkpolicies`.
- Optional `services` get, used for pre-deployment checks; the template does not need to read the Service Secret.

This status does not require:

- Read any Secret.
- Operate a Deployment, Pod or ServiceAccount.
- Operation CRD, ClusterRole or Operator Deployment.
- Proxy access to the Kubernetes API Server.

If you use a public controller such as Argo CD that has cluster-level permissions, you should still limit the actual scope of the release through independent Project, Application permissions or dedicated ServiceAccount; otherwise the Chart is written very narrowly and the execution identity may still be very wide.

## 8. Version differences and pre-adoption checks

Subsequent versions of the official Operator project may include `NetworkEgress`, `ClusterProxy`, routing peer forwarder Service, PDB, and additional RBAC. They will change:

- Number of CRDs and cluster installation permissions.
- Operator ClusterRole resources and verbs.
- `NetworkRouter` derives the number of resources.
- Security context and network behavior of routing peer Pods.
- Requires Gateway API or other external CRD installed.

Therefore, you must also check the following when upgrading:

1. Whether the Helm Chart version, Operator image version and CRD version match.
2. Complete resource list output by `helm template`.
3. The actual `ClusterRole` of the Operator ServiceAccount.
4. Whether the Pod generated by `NetworkRouter` still meets the Pod Security and CNI requirements.
5. Whether the service-only target still does not have `ClusterProxy`, old ingress or whole segment routing enabled.

## 9. Official reference

- [NetBird Helm Chart repository](https://github.com/netbirdio/helms)
- [NetBird Self-hosted Helm Chart](https://github.com/netbirdio/helms/tree/main/charts/netbird)
- [Kubernetes Operator source code](https://github.com/netbirdio/kubernetes-operator)
- [Operator API Reference](https://github.com/netbirdio/kubernetes-operator/blob/main/docs/api-reference.md)
- [Routing Peer](https://docs.netbird.io/use-cases/kubernetes/routing-peer)
- [Route to a Kubernetes Service](https://docs.netbird.io/use-cases/kubernetes/route-to-a-kubernetes-service)
