# Manual `basic-components/test-service:80` access checklist

This document describes the simplest way to deploy a routing peer manually with Kubernetes-native resources, without installing the NetBird Kubernetes Operator.

The goal is to give specified NetBird clients access to:

```text
basic-components/test-service:80/TCP
```

`test-service` still maintains `ClusterIP` and does not create public `Ingress`, `NodePort` or `LoadBalancer`.

## 1. Design and Deployment count

The target cluster only needs to add one Deployment:

```text
netbird-router namespace
├── Secret netbird-setup-key
├── ServiceAccount netbird-router
└── Deployment netbird-router
    └── NetBird routing peer Pod
```

There is no need to add the following Deployment or resources:

- No need to rebuild Deployment for `test-service`.
- No need to create routing peer Service.
- No need to create an Ingress, NodePort or LoadBalancer.
- No need to install `kubernetes-operator` Chart.
- No need to install `NetworkRouter`, `NetworkResource` CRD.
- There is no need to create a Kubernetes `Role` or `ClusterRole` for NetBird routing peers.

If the self-hosted NetBird control plane is not already deployed, Management, Signal, Relay, and Dashboard must still be deployed separately. Those are control-plane resources and are outside the Service access Deployment covered here.

## 2. Expected access link

```text
NetBird client
    |
    | NetBird Overlay
    v
Deployment/netbird-router
    |
    | Kubernetes cluster network
    v
Service/basic-components/test-service:80
    |
    v
Endpoint Pod of test-service
```

The routing peer must be able to access the ClusterIP of `test-service` from its own Pod network namespace. It is not a reverse proxy and does not modify the Service's port or application protocol.

## 3. Pre-implementation inspection

### 3.1 Confirm Service status

```shell
kubectl -n basic-components get service test-service -o wide
kubectl -n basic-components get service test-service -o yaml
kubectl -n basic-components get endpointslice \
  -l kubernetes.io/service-name=test-service
```

Must confirm:

- Service exists and is of type `ClusterIP`.
- Service has non-empty `clusterIP`, recorded as `<TEST_SERVICE_CLUSTER_IP>`.
- `port: 80` exists in `.spec.ports` and the protocol is `TCP`.
- Service has at least one Ready Endpoint.
- `targetPort` points to the port that the application actually listens on.

Get ClusterIP and port mapping:

```shell
kubectl -n basic-components get service test-service \
  -o jsonpath='{.spec.clusterIP}{"\n"}'
kubectl -n basic-components get service test-service \
  -o jsonpath='{range .spec.ports[*]}{.name}{" "}{.port}{" -> "}{.targetPort}{"/"}{.protocol}{"\n"}{end}'
```

### 3.2 Confirm that the routing peer can access the Service

Before creating NetBird resources, first verify that a temporary Pod or subsequent routing peer Pod can access:

```shell
kubectl -n basic-components run service-check --rm -it \
  --restart=Never --image=curlimages/curl:latest -- \
  curl --fail --silent --show-error \
  http://test-service.basic-components.svc.cluster.local:80/
```

If this check fails, you should first check the Service, Endpoint, NetworkPolicy and CNI. You cannot bypass cluster internal network problems by increasing NetBird permissions.

## 4. Kubernetes resource list

### 4.1 Create a private namespace

```shell
kubectl create namespace netbird-router
```

Production environments should have resource quotas, labels, and Pod Security exception approvals set for this namespace. Routing peers require network management capabilities and cannot directly apply baseline policies that do not allow related capabilities.

### 4.2 Create a dedicated ServiceAccount

The routing peer does not require access to the Kubernetes API. Use a standalone ServiceAccount and turn off the automatically mounted ServiceAccount Token:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: netbird-router
  namespace: netbird-router
automountServiceAccountToken: false
```

This ServiceAccount is not bound to `Role`, `RoleBinding`, `ClusterRole` or `ClusterRoleBinding`.

### 4.3 Create Setup Key Secret

Create a dedicated Setup Key in the NetBird control plane:

- The suggested name is `k8s-test-service-router`.
- Enable reusable for re-registration after the Deployment is rebuilt.
- Enable ephemeral peers, suitable for container instances without persistent state.
- Automatically join dedicated Peer Groups, such as `k8s-test-service-routers`.
- Do not use personal login credentials or administrator PAT.

Write the Setup Key to Kubernetes via an external key system or a controlled bootstrap process:

```shell
kubectl -n netbird-router create secret generic netbird-setup-key \
  --from-file=NB_SETUP_KEY=/secure/path/k8s-test-service-router-setup-key
```

If using External Secrets, ensure that the final generated Secret contains the following keys:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: netbird-setup-key
  namespace: netbird-router
type: Opaque
stringData:
  NB_SETUP_KEY: <SETUP_KEY>
```

`<SETUP_KEY>` cannot be committed to Git or written into Deployment YAML.

### 4.4 Create routing peer Deployment

The following is a single-replica test-environment template. Replace `<PINNED_NETBIRD_VERSION>` with a validated pinned version. Self-hosted control planes require `NB_MANAGEMENT_URL`; it can be omitted when using NetBird Cloud.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: netbird-router
  namespace: netbird-router
  labels:
    app.kubernetes.io/name: netbird-router
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: netbird-router
  template:
    metadata:
      labels:
        app.kubernetes.io/name: netbird-router
    spec:
      serviceAccountName: netbird-router
      automountServiceAccountToken: false
      containers:
        - name: netbird
          image: netbirdio/netbird:<PINNED_NETBIRD_VERSION>
          env:
            - name: NB_SETUP_KEY
              valueFrom:
                secretKeyRef:
                  name: netbird-setup-key
                  key: NB_SETUP_KEY
            - name: NB_MANAGEMENT_URL
              value: https://netbird.example.com
            - name: NB_LOG_LEVEL
              value: info
          securityContext:
            capabilities:
              add:
                - NET_ADMIN
                - SYS_RESOURCE
                - SYS_ADMIN
          livenessProbe:
            exec:
              command: ["netbird", "status", "--check", "live"]
            initialDelaySeconds: 5
            periodSeconds: 10
            timeoutSeconds: 5
            failureThreshold: 3
          readinessProbe:
            exec:
              command: ["netbird", "status", "--check", "ready"]
            initialDelaySeconds: 5
            periodSeconds: 10
            timeoutSeconds: 5
            failureThreshold: 3
          startupProbe:
            exec:
              command: ["netbird", "status", "--check", "startup"]
            periodSeconds: 2
            timeoutSeconds: 10
            failureThreshold: 30
```

This template illustrates the following:

- Do not set a fixed `NB_HOSTNAME`, so that after expansion, each Pod will use its own Pod name to register as an independent Peer.
- No Service is required because the client reaches the routing peer through the NetBird Overlay.
- Instead of using `hostNetwork` as the default configuration, first verify whether the Pod network can access the ClusterIP; using host network will expand the node network exposure range.
- If the cluster policy does not allow the above capabilities, platform security approval must be completed first. You cannot simply remove these capabilities and assume that routing will still be available.
- For production environments, adjust `replicas` to 2 or 3, and use topological distribution constraints to spread Pods to different nodes. Do not set a fixed `NB_HOSTNAME` when using multiple copies.

## 5. NetBird control plane configuration

The following configuration is not automatically completed by Kubernetes Deployment and needs to be managed in NetBird Dashboard or through API.

### 5.1 Create a group

Create two private groups:

| Group | Type | Members |
| --- | --- | --- |
| `k8s-test-service-users` | Peer Group | User devices allowed to access the Service |
| `k8s-test-service-routers` | Peer Group | `netbird-router` Deployment registered routing peer |

Setup Key should be configured to automatically add the Peer to `k8s-test-service-routers`.

Do not use `All` as a client group or routing peer group.

### 5.2 Create Network and Resources

Prefer using NetBird's current `Networks` model:

1. Create Network, such as `k8s-basic-components`.
2. Add a routing peer to the Network and select `k8s-test-service-routers`.
3. Create an IP Resource:
   - Name: `basic-components-test-service`.
   - Address: `<TEST_SERVICE_CLUSTER_IP>/32`.
   - Resource Group: `k8s-test-service-resources`.
   - Enable masquerade unless a route back to the NetBird segment has been configured.
4. Create an access policy:
   - Source: `k8s-test-service-users`.
   - Destination: `k8s-test-service-resources`.
   - Protocol: `TCP`.
   - Port: `80`.

Only add the `/32` of this Service, do not add the entire Kubernetes Service CIDR, Pod CIDR or node network segment. NetBird's `/32` Resource can limit access to a single ClusterIP. [NetBird Networks Documentation](https://docs.netbird.io/manage/networks)

If the current control plane version can only use the old `Routes` page, create a Route with the target `<TEST_SERVICE_CLUSTER_IP>/32`, and configure the routing peer group, client distribution group and ACL Group at the same time. The ACL Group cannot be left blank. Routes has been marked as an old model by NetBird, and new configurations will take precedence over Networks. [Routes vs. Networks](https://docs.netbird.io/manage/networks/how-routing-peers-work)

### 5.3 DNS access method

The simplest way to accept is to access directly:

```text
http://<TEST_SERVICE_CLUSTER_IP>:80/
```

If name access is required, you have two options:

- Create a record pointing to the current ClusterIP in NetBird DNS; the Service needs to be updated manually after it is deleted and rebuilt.
- Use routing peer DNS resolution to let the routing peer resolve `test-service.basic-components.svc.cluster.local`; this requires ensuring that the NetBird client, routing peer, and cluster DNS configurations are compatible.

The manual solution does not provide the Operator's Service monitoring or automatic DNS synchronization, so DNS and ClusterIP changes must be handled as part of operations and maintenance.

## 6. Optional Kubernetes NetworkPolicy

If the default deny policy has been used in `basic-components`, you should add rules that allow routing peers to access TCP/80 on the basis of retaining the original business entry rules. The following is for illustration only, the target Pod label must be replaced with the label actually selected by `test-service`:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: test-service-from-netbird
  namespace: basic-components
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: test-service
  policyTypes:
    - Ingress
  ingress:
    - from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: netbird-router
          podSelector:
            matchLabels:
              app.kubernetes.io/name: netbird-router
      ports:
        - protocol: TCP
          port: 80
```

Confirm the source identity seen by the CNI before applying the policy. The routing peer may masquerade forwarded traffic by default, so the target Pod may see the routing peer Pod IP, node IP, or another address. If the actual source identity is not represented by a Pod label, adjust the rules to match the CNI behavior instead of allowing the entire network segment.

## 7. Client use

### 7.1 Install and connect NetBird client

Install a control plane-compatible NetBird client on the workstation that needs to access the Service. Desktop client and command line client are supported. For the installation entry, see [NetBird Installation Document](https://docs.netbird.io/get-started/install).

When using the command line client for a self-hosted control plane:

```shell
netbird up --management-url https://netbird.example.com
```

`--management-url` can be omitted when using NetBird Cloud:

```shell
netbird up
```

Desktop client connection method:

1. Open the NetBird client.
2. Select Sign in or Connect to a self-hosted deployment.
3. For self-hosted deployment, fill in `https://netbird.example.com`.
4. Complete login and MFA through your organization's OIDC identity provider.

User devices should use interactive OIDC login and should not use the routing peer's Setup Key. Setup Key is only used for unattended cluster routing peers.

### 7.2 Join the client group that allows access

After the client logs in, the platform administrator needs to add the Peer:

```text
k8s-test-service-users
```

If group membership is automatically synchronized by OIDC or other identity systems, then authorization is done through the user identity and do not manually add the user device to the `All` group.

Confirm client status:

```shell
netbird status -d
```

Need to confirm:

- Peer status is connected.
- The Management URL is the target NetBird control plane.
- The client has received the corresponding Network Resource or route.
- The client belongs to `k8s-test-service-users`, or to the parent identity group actually used by this group.

You can view the currently received Networks configuration:

```shell
netbird networks ls
```

If the client is online but cannot see `<TEST_SERVICE_CLUSTER_IP>/32`, first check whether the Peer group, Network resources, routing peer are online and whether the access policy has taken effect.

### 7.3 Access `test-service`

Directly use the Service's ClusterIP to access TCP/80:

```shell
curl --fail --silent --show-error \
  http://<TEST_SERVICE_CLUSTER_IP>:80/
```

If you need to view the response headers or troubleshoot the connection process:

```shell
curl --verbose \
  http://<TEST_SERVICE_CLUSTER_IP>:80/
```

The port must use the application protocol actually provided by the Service. TCP/80 is usually HTTP, but NetBird will not automatically convert HTTP to HTTPS; if the application uses other TCP protocols on port 80, it should be tested with the corresponding client tools.

If NetBird DNS records are configured, you can use record access:

```shell
getent hosts <TEST_SERVICE_DNS_NAME>
curl --fail --silent --show-error \
  http://<TEST_SERVICE_DNS_NAME>:80/
```

If the cluster DNS resolution method mentioned earlier in the document is used, the name is usually:

```text
test-service.basic-components.svc.cluster.local
```

Whether this name can be resolved on the client depends on NetBird routing peer DNS resolution and the cluster DNS configuration. It cannot be assumed that all clients can resolve the `cluster.local` domain name by default. It is recommended to use ClusterIP for the first acceptance, confirm that routing and policies are normal, and then verify DNS.

### 7.4 Client usage boundaries

- NetBird only provides network reachability and cannot replace `test-service`'s own login, token, Basic Auth or mTLS.
- Clients can only access destinations and ports allowed by NetBird policy.
- Accessing other Services, Pod IPs, node IPs or Kubernetes APIs is not within the scope of authorization of this plan.
- Clients that have not joined `k8s-test-service-users` should not be able to access this Resource even if NetBird is installed.
- Clients do not need to install Kubernetes CLI or save kubeconfig; this solution does not provide Kubernetes API access.

### 7.5 User or device revocation

When a user leaves the company, the device is lost, or access is no longer required, administrators should do the following:

1. Remove the Peer or corresponding user from `k8s-test-service-users`.
2. If necessary, delete the Peer in the NetBird control plane.
3. Check the access policy and activity logs to confirm that the device is no longer establishing connections to the Resource.
4. If the device key is leaked, re-register the device according to the NetBird client life cycle and clean up the old peers.

Only revoking the Setup Key of the routing peer will not automatically revoke the registered user client; the Setup Key and the user Peer are two different sets of identities.

## 8. Deployment sequence

- [ ] Verify that `basic-components/test-service` is `ClusterIP`, port is TCP/80, and has Ready Endpoint.
- [ ] Confirm that the routing peer Pod network can access the Service ClusterIP.
- [ ] Confirm that the NetBird control plane is available and the client is registered.
- [ ] Create two private groups `k8s-test-service-routers` and `k8s-test-service-users`.
- [ ] Create reusable and ephemeral Setup Key, and automatically join the routing peer group.
- [ ] Create `netbird-router` namespace and ServiceAccount without Kubernetes API permissions.
- [ ] Write Setup Key to `netbird-router/netbird-setup-key`.
- [ ] Apply routing peer Deployment.
- [ ] Confirm that the Pod is Ready and appears as an online routing peer in NetBird Dashboard.
- [ ] Create `k8s-basic-components` Network, routing peer and `<TEST_SERVICE_CLUSTER_IP>/32` Resource.
- [ ] Create a policy that only allows the client group access to the resource group TCP/80.
- [ ] If default-deny `NetworkPolicy` is present, merge rules that allow routing peers to target Pod TCP/80.
- [ ] Test access from authorized clients using ClusterIP and TCP/80.
- [ ] Unable to establish TCP/80 connection confirmed by unauthorized client.
- [ ] Record ClusterIP, NetBird Resource, policy and Deployment versions to include in subsequent change management.

## 9. Acceptance commands

Kubernetes side:

```shell
kubectl -n netbird-router get deployment,pod -o wide
kubectl -n netbird-router logs deploy/netbird-router
kubectl -n basic-components get service test-service -o wide
kubectl -n basic-components get endpointslice \
  -l kubernetes.io/service-name=test-service
```

Check routing peer status:

```shell
kubectl -n netbird-router exec deploy/netbird-router -- \
  netbird status -d
```

Authorize the NetBird client:

```shell
netbird status -d
curl --fail --silent --show-error \
  http://<TEST_SERVICE_CLUSTER_IP>:80/
```

If custom DNS is configured, additionally verify name resolution and access. Unauthorized clients must not be able to access this Resource; access to the routing peer's own NetBird IP should not be considered an acceptance condition for access to `test-service`.

## 10. Subsequent maintenance

- Endpoint Pod changes of `test-service` do not require modification of NetBird configuration, as long as the ClusterIP remains unchanged.
- Deleting and rebuilding the Service may cause ClusterIP changes; NetBird Resource and DNS records must be updated simultaneously.
- Before upgrading the routing peer Deployment image, network capabilities, probes, and routing behavior should be verified in the test cluster.
- When Setup Key is rotated, first create a new Key and update the Secret, then restart the Deployment on a rolling basis, and finally revoke the old Key.
- If the number of Services increases, continue to create a separate `/32` Resource for each Service; do not publish the entire Service CIDR to reduce configuration.
- When the number of Services or the frequency of changes increases, you should migrate to the Operator's `NetworkRouter`/`NetworkResource` solution and let the Operator automatically synchronize ClusterIP and DNS.

## 11. References

- [Deploying routing peers in Kubernetes](https://docs.netbird.io/use-cases/kubernetes/routing-peers-and-kubernetes)
- [NetBird Networks](https://docs.netbird.io/manage/networks)
- [NetBird routing peer principle](https://docs.netbird.io/manage/networks/how-routing-peers-work)
- [Route to a Kubernetes Service](https://docs.netbird.io/use-cases/kubernetes/route-to-a-kubernetes-service)
