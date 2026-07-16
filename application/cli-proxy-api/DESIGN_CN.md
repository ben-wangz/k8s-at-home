# CLIProxyAPI Helm Chart 可行性评估与实施方案

## 1. 文档状态

- 目标目录：`application/cli-proxy-api/`
- 本阶段范围：可行性评估和方案设计
- 本阶段不创建 Helm chart 模板，不部署到 Kubernetes
- CLIProxyAPI 评估基线：`v7.2.80`，commit `09da52ad`
- 参考 application：`application/sshpiper/`、`application/codespace/`、`application/sub2api/`
- 原始部署手册：工作区根目录 `CLIProxyAPI_DEPLOYMENT_GUIDE_CN.md`

## 2. 最终结论

将 CLIProxyAPI 固化为 Helm chart 可行。最终采用以下架构：

```text
Helm values（非敏感配置）
  -> ConfigMap: config-base.json

Secret（仅敏感字段）
  -> api-keys

ConfigMap + Secret
  -> init container: render-config
  -> emptyDir: /runtime/config.yaml

客户端
  -> 用户管理的 Traefik HTTPS Ingress
  -> ClusterIP Service :8317
  -> 单副本 CLIProxyAPI Deployment :8317
       |- /runtime/config.yaml（init container 生成）
       `- /data/auths（PVC，OAuth 动态状态）
  -> xAI OAuth / xAI API
```

核心决策：

| 主题 | 方案 |
| --- | --- |
| 运行镜像 | 维护一个基于上游 CLIProxyAPI 镜像的轻量派生镜像 |
| 运维脚本 | 直接打入派生镜像 |
| 应用端口 | 固定为 `8317`，不提供 values 配置项 |
| 非敏感配置 | 由 values 控制并渲染为 ConfigMap 基线 |
| 敏感配置 | Secret 只保存 CPA API Key 等敏感字段，不保存完整配置文件 |
| 完整配置 | init container 将 ConfigMap 与 Secret 合成为 emptyDir 中的 `config.yaml` |
| OAuth 状态 | 使用 PVC 持久化，不能直接使用 Secret volume 代替 |
| 首次 OAuth | 安装后通过 `kubectl exec -it` 启动 device login，人工在浏览器授权 |
| Ingress | 标准 Kubernetes Ingress，面向 Traefik，模板结构参考 `sub2api` |
| TLS | 完全由用户现有 Traefik/Ingress 配置管理，chart 不创建证书 |
| 工作负载 | 单副本 `Deployment`，`Recreate` 更新策略 |
| 通用模板 | 依赖 bitnami common `2.x.x` |

## 3. 源码依据

方案依据远端 CLIProxyAPI 源码中的以下行为：

1. 上游提供 `docker.io/eceasy/cli-proxy-api` 多架构镜像，并按 Git tag 发布固定版本。
2. `config.example.yaml` 支持显式 `auth-dir`，Kubernetes 中可固定为 `/data/auths`。
3. `internal/cmd/xai_login.go` 和 `sdk/auth/xai.go` 使用 xAI device authorization flow，输出验证网址和 user code 后等待人工授权。
4. `sdk/auth/filestore.go` 将 OAuth 凭据以 `0600` JSON 文件写入认证目录。
5. `internal/auth/xai/xai.go` 支持 refresh token 刷新 access token。
6. token 刷新后会持久化更新的认证数据，因此认证目录必须可写。
7. `internal/runtime/executor/xai_executor.go` 使用认证记录中的 `using_api` 决定 OAuth 请求的 xAI 路由。
8. CLIProxyAPI 使用 YAML 配置；JSON 是 YAML 的兼容子集，实施阶段仍需通过固定版本镜像验证生成的 JSON 内容能被配置加载器正常读取。

## 4. 自维护运行镜像

### 4.1 目的

第一版允许维护一个基于上游固定版本镜像的 container image。它只增加部署所需的工具和脚本，不修改 CLIProxyAPI 源码或替换上游二进制。

镜像职责：

- 保留上游 `/CLIProxyAPI/CLIProxyAPI` 二进制和默认启动命令。
- 安装 `jq`，用于结构化合成 JSON/YAML 配置和修改 OAuth JSON。
- 内置 `cli-proxy-api-render-config`。
- 内置 `cli-proxy-api-xai-login`。
- 内置可选的非敏感诊断脚本，但不得输出 token 或 API Key。

### 4.2 目录结构

```text
application/cli-proxy-api/container/
|- Containerfile
|- VERSION
`- bin/
   |- cli-proxy-api-render-config
   `- cli-proxy-api-xai-login
```

### 4.3 Containerfile 方案

```dockerfile
ARG CLIPROXYAPI_IMAGE=docker.io/eceasy/cli-proxy-api:v7.2.80

FROM ${CLIPROXYAPI_IMAGE}

RUN apt-get update \
    && apt-get install -y --no-install-recommends jq \
    && rm -rf /var/lib/apt/lists/*

COPY --chmod=0755 application/cli-proxy-api/container/bin/ /usr/local/bin/
```

要求：

- 上游基础镜像必须固定 tag，不使用 `latest`。
- 派生镜像通过本仓库现有 `release-container` 流程发布到 GHCR。
- `Containerfile` 不覆盖上游 `CMD`，主容器仍直接启动 CLIProxyAPI。
- 脚本使用 POSIX shell，能从绝对路径执行，不依赖当前工作目录。
- 脚本不接受密钥作为命令行参数，避免出现在进程参数中。
- 镜像只增加部署工具，不引入反向代理 sidecar 或 Kubernetes API 客户端。

### 4.4 forgekit 集成

Chart annotation 按 `sshpiper` 的方式关联派生镜像：

```yaml
annotations:
  cli-proxy-api/images: |
    - name: cli-proxy-api-runtime
      path: application/cli-proxy-api/container
      valuesKey: image.tag
```

容器和 chart 的版本号必须通过 `forgekit` 管理，不手工修改版本字段。

## 5. 配置生成方案

### 5.1 配置分层

配置严格拆成三层：

| 层 | 资源 | 内容 | 是否敏感 |
| --- | --- | --- | --- |
| 基线配置 | ConfigMap | host、port、auth-dir、日志、重试、路由等 | 否 |
| 凭据 | Secret | CPA API Key 列表 | 是 |
| 最终配置 | emptyDir | init container 合并后的完整配置 | 是 |

Secret 不承载 `config.yaml` 裸文件，也不包含 host、port、Ingress、日志等非敏感配置。

### 5.2 Secret 契约

生产部署必须预先创建一个 Opaque Secret：

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: <release-name>-credentials
type: Opaque
stringData:
  api-keys: '["<CPA_API_KEY>"]'
```

这里只定义 Secret 的数据结构，不在文档、values 或 Git 中写入真实值。

`api-keys` 的内容为 JSON 字符串数组，支持一个或多个 CPA 客户端 Key。init container 必须校验：

- 顶层类型是数组。
- 数组至少包含一个元素。
- 每个元素都是非空字符串。
- 校验失败时终止 Pod 启动，但错误信息不能输出 Key 内容。

values 只保存 Secret 引用和键名：

```yaml
credentials:
  existingSecret: ""
  secretKeys:
    apiKeys: api-keys
```

`credentials.existingSecret` 默认必填。第一版不创建包含内联密钥的 Secret，避免真实 Key 进入 values 和 Helm release 数据。

### 5.3 非敏感 values

host、port、TLS、auth directory 等部署边界不进入 Secret，其中部分保持固定：

```yaml
host: ""
port: 8317
auth-dir: /data/auths
tls:
  enable: false
```

可调非敏感参数由 values 控制，例如：

```yaml
config:
  debug: false
  loggingToFile: false
  logsMaxTotalSizeMB: 0
  errorLogsMaxFiles: 10
  usageStatisticsEnabled: false
  requestRetry: 3
  maxRetryCredentials: 0
  maxRetryInterval: 30
  disableCooling: false
  wsAuth: true
  routing:
    strategy: round-robin
    sessionAffinity: false
    sessionAffinityTTL: 1h
  streaming:
    keepaliveSeconds: 15
    bootstrapRetries: 1
  extra: {}
```

`config.extra` 仅用于 CLIProxyAPI 的其他非敏感字段。模板必须禁止它覆盖以下保留字段：

- `host`
- `port`
- `tls`
- `auth-dir`
- `api-keys`
- `remote-management.secret-key`
- 任何 provider API Key 字段

含用户名、密码或 token 的代理 URL 也不能写入 `config.extra`。以后确实需要更多敏感配置时，应扩展 Secret 契约和 renderer，而不是把密钥塞进 values。

### 5.4 ConfigMap 基线

Chart 将固定字段和 `.Values.config` 渲染为 `config-base.json`。使用 JSON 而不是字符串拼接 YAML，便于 init container 用 `jq` 做结构化校验和合并。

基线配置至少包含：

```json
{
  "host": "",
  "port": 8317,
  "tls": {
    "enable": false,
    "cert": "",
    "key": ""
  },
  "remote-management": {
    "allow-remote": false,
    "secret-key": "",
    "disable-control-panel": true
  },
  "auth-dir": "/data/auths",
  "api-keys": [],
  "logging-to-file": false,
  "usage-statistics-enabled": false,
  "ws-auth": true
}
```

ConfigMap 不包含任何真实 API Key、OAuth token 或 TLS 私钥。

### 5.5 init container 合成

Pod 使用三个 volume：

```text
config-base    ConfigMap   -> /input/config/config-base.json (read-only)
credentials    Secret      -> /input/credentials/api-keys  (read-only)
runtime-config emptyDir    -> /runtime                     (read-write)
```

init container 和主容器使用同一个派生镜像。init container 执行：

```text
/usr/local/bin/cli-proxy-api-render-config
```

renderer 的逻辑：

1. 读取 `/input/config/config-base.json`。
2. 使用 `jq` 验证 `/input/credentials/api-keys` 是合法的非空字符串数组。
3. 把 API Key 数组写入基线配置的 `api-keys` 字段。
4. 将结果写入临时文件。
5. 使用 `jq -e` 再次验证最终 JSON。
6. 以 `0600` 权限原子移动为 `/runtime/config.yaml`。
7. 不向 stdout/stderr 输出最终配置或 Secret 内容。

主容器只挂载：

```text
/runtime/config.yaml -> /CLIProxyAPI/config.yaml
```

主容器无需直接挂载 credentials Secret。完整配置只存在于 Pod 的 emptyDir 中，Pod 删除后随之销毁，并在下一个 Pod 启动时重新生成。

### 5.6 配置变更和 Key 轮换

- ConfigMap 内容变化后，需要创建新 Pod 才会重新执行 init container。
- existing Secret 内容变化后，也需要创建新 Pod 才会生成包含新 Key 的配置。
- Chart-generated ConfigMap 使用 checksum pod annotation 自动触发 rollout。
- external Secret 无法在 Helm render 时计算内容 checksum；Key 轮换后由 Secret 管理器触发 rollout，或人工执行 `kubectl rollout restart`。
- chart 不授予 Pod 读取 Kubernetes API 或更新 Secret 的权限。

## 6. OAuth 状态与 PVC

### 6.1 为什么必须使用 PVC

OAuth JSON 包含 access token、refresh token、ID token 和账户标识，而且 CLIProxyAPI 会在 token 刷新后写回文件。它是敏感的动态状态，不是静态配置。

Kubernetes Secret volume 是只读投影，无法承载 CLIProxyAPI 的运行时刷新写入。认证目录必须挂载可写 PVC：

```text
/data/auths
```

### 6.2 values 设计

完整模仿 `codespace` 的 StorageClass 和 existingClaim 模式：

```yaml
persistence:
  auth:
    enabled: true
    storageClass: ""
    existingClaim: ""
    accessModes:
      - ReadWriteOnce
    size: 1Gi
    subPath: ""
    annotations: {}
    labels: {}
    selector: {}
    dataSource: {}
```

行为：

- `enabled: true` 且未指定 `existingClaim` 时创建 PVC。
- `storageClass: ""` 时使用集群默认 StorageClass。
- 设置 `storageClass` 时写入 `storageClassName`。
- 设置 `existingClaim` 时复用现有 PVC。
- `enabled: false` 时使用 `emptyDir`，仅用于临时测试，Pod 重建后必须重新 OAuth。

PVC 包含高敏感凭据，应启用存储侧加密、严格 RBAC 和加密备份。

## 7. 首次人工 OAuth

### 7.1 操作入口

Helm 安装不创建等待人工操作的 hook 或 Job。Pod 正常启动后，由操作者执行：

```bash
kubectl exec -it \
  --namespace <namespace> \
  deployment/<release-fullname> \
  -- /usr/local/bin/cli-proxy-api-xai-login
```

镜像中的脚本执行：

```bash
/CLIProxyAPI/CLIProxyAPI \
  --xai-login \
  --no-browser \
  --config /CLIProxyAPI/config.yaml
```

### 7.2 人工步骤

1. 从终端读取完整验证网址和 device code。
2. 在浏览器登录实际持有 SuperGrok 的账户。
3. 输入 device code 并确认授权。
4. 保持终端连接，等待认证文件写入 `/data/auths`。
5. 登录脚本使用 `jq` 幂等设置 xAI OAuth 文件的 `using_api: true`。
6. 确认认证文件存在，但不输出文件内容。

values 控制当前版本需要的路由兼容处理：

```yaml
xai:
  usingAPI: true
```

Deployment 将该非敏感开关作为环境变量传给登录脚本。

脚本要求：

- 不打印 OAuth JSON。
- 不打印 access token、refresh token、ID token 或 API Key。
- 只处理 `/data/auths/xai-*.json`。
- 使用 `jq` 做结构化更新，不使用 `sed` 修改 JSON。
- 通过临时文件和 `mv` 原子替换。
- 保持认证文件权限为 `0600`。
- CLIProxyAPI 登录命令即使只记录错误而返回成功时，仍通过认证文件存在性和 JSON 字段校验判断最终结果。

### 7.3 为什么不用 Helm hook

device flow 需要人在有限时间内完成浏览器确认。放进 Helm hook 或自动 Job 会造成：

- Helm wait 超时。
- Job 重试生成多个 device code。
- device code 和 URL 进入集中日志。
- GitOps 控制器反复重建一次性登录资源。
- 每次升级和一次性账户引导错误耦合。

因此，显式 `kubectl exec -it` 是唯一默认登录路径。

## 8. Pod 重启和迁移

### 8.1 登录过程中重启

如果 Pod 或 exec 会话在 device flow 完成前中断，当前轮询进程终止。重新执行登录脚本并使用新的 device code 即可。

### 8.2 登录完成后重启

认证文件写入 PVC 后，正常 Pod 重启不需要再次 device login：

- Pod 名称和 UID 不参与 OAuth 文件持久化。
- access token 到期后使用 refresh token 自动刷新。
- 刷新后的 token 继续写回同一 PVC。

### 8.3 节点迁移

只要 StorageClass/CSI 驱动能将 RWO 卷从旧节点卸载并挂载到新节点，Deployment 迁移后不需要重新登录。卷的可用区限制和故障恢复能力由 StorageClass 决定。

### 8.4 跨集群恢复

迁移认证数据时：

1. 先将旧环境缩容为 `0`。
2. 恢复最新的 auth PVC 数据。
3. 保持文件权限和目录所有权。
4. 启动新环境并执行最小模型调用。
5. 只有 refresh token 失效、会话被撤销或备份过旧时才重新 device login。

不能让新旧环境同时使用同一份复制出来的 refresh token 状态，否则可能发生 token 轮换竞争和旧 token 重放。

## 9. Deployment 与副本策略

工作负载使用 `Deployment`：

- 服务不依赖稳定 Pod 名称或稳定网络身份。
- 持久状态由独立 PVC 提供。
- 与 `codespace` 的 Deployment + PVC 模式一致。

默认：

```yaml
replicas: 1
updateStrategy:
  type: Recreate
```

文件型 OAuth token store 按单写者使用。单副本和 `Recreate` 可以避免：

- 两个 Pod 同时刷新和覆盖 refresh token。
- RWO PVC 在滚动升级中的多重挂载等待。
- 登录运维脚本选择到不同 Pod。

`_helpers.tpl` 必须校验 `replicas == 1`。未来只有切换并验证 CLIProxyAPI 的外部存储后才考虑多副本。

## 10. 固定应用端口

CLIProxyAPI 内部监听端口固定为 `8317`。values schema 不定义应用端口或 Service 端口字段。

端口在 `_helpers.tpl` 中定义一次：

```gotemplate
{{- define "cli-proxy-api.port" -}}8317{{- end -}}
```

以下位置统一引用该 helper：

- ConfigMap 的 `port`。
- Deployment 的 `containerPort`。
- TCP probes。
- Service 的 `port` 和 `targetPort`。
- Ingress backend 的 Service port。

Service 默认使用 `ClusterIP`。对外访问完全由 Traefik Ingress 负责。

## 11. Traefik Ingress

### 11.1 设计原则

Ingress 模板直接参考 `application/sub2api/chart/templates/ingress.yaml`：

- 使用标准 Kubernetes `Ingress`。
- 使用 `common.capabilities.ingress.apiVersion`。
- 使用 `common.ingress.backend`。
- 合并 `ingress.annotations` 和 `commonAnnotations`。
- 支持 hostname、path、pathType、extraHosts、extraPaths、extraRules 和 extraTls。
- backend 指向固定端口 `8317` 的 ClusterIP Service。

Traefik 通过 Kubernetes Ingress provider 监听标准 Ingress 资源。TLS 可以由用户已有的 Traefik entryPoint、证书解析器、Ingress annotations 或 `spec.tls` 处理。

### 11.2 values 草案

```yaml
ingress:
  enabled: false
  ingressClassName: traefik
  hostname: cli-proxy-api.example.internal
  path: /
  pathType: Prefix
  annotations: {}
  tls: false
  tlsSecret: ""
  extraHosts: []
  extraPaths: []
  extraTls: []
  extraRules: []
```

chart 只渲染标准 Ingress 及用户提供的 Traefik annotations/TLS 引用，不创建 TLS Secret，也不假设 Traefik entryPoint 或证书管理方式。

用户根据自己的 Traefik 环境设置 annotations 和 TLS。例如，若环境通过 annotations 选择 entryPoint 或 certificate resolver，直接写入 `ingress.annotations`；若使用已有 TLS Secret，则设置 `ingress.tls` 和 `ingress.tlsSecret`。

### 11.3 SSE 与 WebSocket

CLIProxyAPI 的 SSE 和 WebSocket 仍通过标准 HTTP Service 转发。Chart 不内置控制器特有的超时对象；如用户环境需要 Traefik Middleware、ServersTransport 或其他 CRD，可通过：

- `ingress.annotations`
- `ingress.extraRules`
- `extraDeploy`

进行配置。

Traefik Kubernetes Ingress 参考：

- <https://doc.traefik.io/traefik/providers/kubernetes-ingress/>
- <https://doc.traefik.io/traefik/reference/routing-configuration/kubernetes/ingress/>

## 12. Chart 目录规划

```text
application/cli-proxy-api/
|- README.md
|- DESIGN_CN.md
|- container/
|  |- Containerfile
|  |- VERSION
|  `- bin/
|     |- cli-proxy-api-render-config
|     `- cli-proxy-api-xai-login
`- chart/
   |- .gitignore
   |- Chart.yaml
   |- values.yaml
   `- templates/
      |- NOTES.txt
      |- _helpers.tpl
      |- auth-pvc.yaml
      |- configmap.yaml
      |- deployment.yaml
      |- extra-list.yaml
      |- ingress.yaml
      `- service.yaml
```

credentials Secret 和 TLS 配置由用户管理；OAuth 引导通过运行中 Pod 的内置脚本完成。Chart 只创建上述目录中列出的 Kubernetes 资源。

## 13. values 结构草案

```yaml
replicas: 1

commonLabels: {}
commonAnnotations: {}
podLabels: {}
podAnnotations: {}

revisionHistoryLimit: 10
updateStrategy:
  type: Recreate

imagePullSecrets: []
affinity: {}
podAffinityPreset: ""
podAntiAffinityPreset: soft
nodeAffinityPreset:
  type: none
  key: ""
  values: []

podSecurityContext:
  enabled: true
  fsGroupChangePolicy: Always
  supplementalGroups: []
  fsGroup: 1001

containerSecurityContext:
  enabled: true
  runAsUser: 1001
  runAsGroup: 1001
  runAsNonRoot: true
  allowPrivilegeEscalation: false
  readOnlyRootFilesystem: false
  capabilities:
    drop:
      - ALL

nodeSelector: {}
tolerations: []

image:
  registry: ghcr.io
  repository: ben-wangz/k8s-at-home-cli-proxy-api-runtime
  tag: ""
  pullPolicy: IfNotPresent

credentials:
  existingSecret: ""
  secretKeys:
    apiKeys: api-keys

config:
  debug: false
  loggingToFile: false
  logsMaxTotalSizeMB: 0
  errorLogsMaxFiles: 10
  usageStatisticsEnabled: false
  requestRetry: 3
  maxRetryCredentials: 0
  maxRetryInterval: 30
  disableCooling: false
  wsAuth: true
  routing:
    strategy: round-robin
    sessionAffinity: false
    sessionAffinityTTL: 1h
  streaming:
    keepaliveSeconds: 15
    bootstrapRetries: 1
  extra: {}

xai:
  usingAPI: true

persistence:
  auth:
    enabled: true
    storageClass: ""
    existingClaim: ""
    accessModes:
      - ReadWriteOnce
    size: 1Gi
    subPath: ""
    annotations: {}
    labels: {}
    selector: {}
    dataSource: {}

service:
  type: ClusterIP
  annotations: {}
  clusterIP: ""
  sessionAffinity: ""
  sessionAffinityConfig: {}
  externalTrafficPolicy: ""
  loadBalancerSourceRanges: []
  loadBalancerIP: ""
  loadBalancerClass: ""
  extraPorts: []

ingress:
  enabled: false
  ingressClassName: traefik
  hostname: cli-proxy-api.example.internal
  path: /
  pathType: Prefix
  annotations: {}
  tls: false
  tlsSecret: ""
  extraHosts: []
  extraPaths: []
  extraTls: []
  extraRules: []

resources: {}
resourcesPreset: nano

customLivenessProbe: {}
customReadinessProbe: {}
customStartupProbe: {}

extraEnvVars: []
extraEnvVarsSecret: ""
extraVolumeMounts: []
extraVolumes: []
extraDeploy: []
```

`image.tag` 的实际默认值由 forgekit 与 container `VERSION` 同步，不手工维护。

## 14. bitnami common 复用

`Chart.yaml` 依赖与参考 chart 一致：

```yaml
dependencies:
  - name: common
    repository: oci://registry-1.docker.io/bitnamicharts
    tags:
      - bitnami-common
    version: 2.x.x
```

模板复用：

| 能力 | common helper |
| --- | --- |
| Deployment API 版本 | `common.capabilities.deployment.apiVersion` |
| Ingress API 版本 | `common.capabilities.ingress.apiVersion` |
| 名称和 namespace | `common.names.fullname`、`common.names.namespace` |
| 标准标签和 selector | `common.labels.standard`、`common.labels.matchLabels` |
| 镜像拼装 | `common.images.image` |
| affinity preset | `common.affinities.pods`、`common.affinities.nodes` |
| securityContext | `common.compatibility.renderSecurityContext` |
| 资源预设 | `common.resources.preset` |
| annotations/extra values | `common.tplvalues.render`、`common.tplvalues.merge` |
| Ingress backend | `common.ingress.backend` |

CLIProxyAPI 自有 helpers 只负责：

- 固定端口 `8317`。
- credentials Secret 名称和键名。
- auth PVC 名称。
- config ConfigMap 名称。
- values 必填、互斥和保留字段校验。
- 单副本校验。

## 15. 模板职责

### `configmap.yaml`

- 将固定配置和 `.Values.config` 渲染为 `config-base.json`。
- 使用结构化 Helm dict/toJson，不通过字符串替换组装敏感值。
- 不包含 `api-keys` 的真实内容。
- 不包含 OAuth 数据和 TLS 数据。

### `deployment.yaml`

- 调用 `cli-proxy-api.validateValues`。
- 使用 common 渲染名称、标签、镜像、affinity、securityContext 和 resources。
- 使用同一派生镜像运行 init container 和主容器。
- init container 挂载 ConfigMap、credentials Secret 和 runtime emptyDir。
- 主容器只挂载生成后的 runtime config 和 auth PVC。
- 配置 ConfigMap checksum 进入 pod annotations。
- 暴露固定命名端口 `http: 8317`。
- 默认使用 TCP startup/readiness/liveness probes，避免带认证的 HTTP 路径影响探针。
- 支持 custom probes、extra env Secret、extra mounts 和 extra volumes。

### `service.yaml`

- 默认 `ClusterIP`。
- Service `port` 和 `targetPort` 固定为 `8317`。
- 使用 `sshpiper`/`sub2api` 相同的 annotation merge 和 selector merge 模式。
- 保留 Service 类型和常见扩展字段，但不提供端口 values。

### `ingress.yaml`

- 直接参考 `sub2api` Ingress 模板。
- backend Service port 使用固定端口 helper。
- 默认 `ingressClassName: traefik`。
- 支持用户已有 TLS 和 Traefik annotations。
- 不创建或管理证书。

### `auth-pvc.yaml`

- 模仿 `codespace` PVC 字段。
- 仅在 `enabled=true` 且 `existingClaim` 为空时创建。

### `NOTES.txt`

- 给出首次 `kubectl exec -it` device login 命令。
- 给出 Secret 结构要求，但不读取或输出 Secret 内容。
- 提示 credentials Secret 轮换后需要 rollout。
- 给出检查 auth 文件是否存在的命令，但不输出文件内容。
- 不输出 CPA API Key、OAuth token 或 TLS 信息。

### `extra-list.yaml`

与 `sshpiper`、`codespace`、`sub2api` 一致，使用 `common.tplvalues.render` 渲染 `extraDeploy`。

## 16. 安全上下文

目标是以 UID/GID `1001` 运行 init container、主容器和 exec 登录脚本：

- credentials Secret volume 使用 `defaultMode: 0440`。
- Pod `fsGroup: 1001` 为 init container 提供 Secret 只读权限和 PVC 写权限。
- runtime config 由 UID `1001` 创建并设置为 `0600`。
- auth JSON 由 UID `1001` 创建并设置为 `0600`。
- drop `ALL` Linux capabilities。
- 禁止 privilege escalation。

实施阶段必须验证上游基础镜像在非 root 用户下运行 CLIProxyAPI 和 device login 的兼容性。如果固定版本镜像确实依赖 root，应以实际验证结果调整默认 securityContext，并记录原因；不能只为了形式上的非 root 破坏服务。

## 17. 安装与首次启用顺序

1. 准备 namespace。
2. 现场生成一个或多个 CPA API Key。
3. 创建只包含 `api-keys` 字段的 credentials Secret。
4. 确认默认 StorageClass，或设置 `persistence.auth.storageClass`/`existingClaim`。
5. 按现有 Traefik 环境填写 Ingress hostname、annotations 和 TLS 配置。
6. 安装 chart。
7. init container 生成完整配置，主容器启动。
8. 等待 TCP probes 就绪。
9. 执行 `kubectl exec -it ... cli-proxy-api-xai-login`。
10. 人工完成 device verification。
11. 确认 auth PVC 中存在 xAI JSON，但不输出内容。
12. 使用 CPA API Key 调用 `/v1/models`。
13. 执行 `grok-4.5` 最小文本请求。
14. 可选验证 `grok-build-0.1`；图片生成测试只有在接受额度消耗时执行。

## 18. 安全与备份

- credentials Secret 只包含 API Key，不包含普通配置。
- 最终完整配置位于 Pod emptyDir，Pod 删除后销毁。
- OAuth PVC 按 Secret 同等级别保护。
- 默认关闭 remote management 和 control panel。
- 应用原生 TLS 固定关闭，由 Traefik 终止 TLS。
- Service 默认 ClusterIP。
- 不授予应用 ServiceAccount 读取 Secret API 或修改集群资源的权限。
- 不在 logs、NOTES、probe 或运维脚本中输出凭据。
- 建议使用支持快照的 StorageClass，并加密备份 auth PVC。
- 恢复前停止旧实例，避免两个环境同时刷新同一 token。
- Helm uninstall 前确认 PVC 备份和保留策略。
- NetworkPolicy 可通过 `extraDeploy` 提供，后续再按仓库统一模式标准化。

## 19. 实施步骤

1. 使用 forgekit 注册 `cli-proxy-api` chart 和 `cli-proxy-api-runtime` container 版本关系。
2. 创建派生镜像目录、Containerfile 和两个脚本。
3. 创建 Chart.yaml、values.yaml 和 bitnami common 依赖。
4. 实现固定端口、Secret、PVC 和 values 校验 helpers。
5. 实现 config ConfigMap 和 init container 配置合成。
6. 实现 Deployment、Service 和 PVC。
7. 按 `sub2api` 实现 Traefik Ingress。
8. 实现 NOTES 和 README，明确人工 OAuth 步骤。
9. 更新 `version-control.yaml`、根 README 和 lint workflow。
10. 增加 renderer 脚本测试，覆盖合法/空数组/错误 JSON/非字符串元素/密钥不泄露。
11. 增加 xAI login 脚本测试，覆盖幂等 `using_api` 修改和失败检测。
12. 增加 Helm render 场景：credentials Secret、PVC、existingClaim、Traefik Ingress TLS、extraDeploy 和保留字段校验。
13. 获得测试许可后运行 shell 测试、容器构建、Helm lint/template 和 forgekit lint。
14. 在测试 namespace 完成一次真实 device flow、Pod 重启和节点迁移验证。

## 20. 验收标准

- [ ] 运行镜像继承固定版本上游 CLIProxyAPI 镜像。
- [ ] 运维脚本位于派生镜像。
- [ ] credentials Secret 只保存 `api-keys` 敏感字段。
- [ ] host、port、日志、路由等普通配置不进入 Secret。
- [ ] init container 使用结构化工具生成完整配置。
- [ ] 主容器不直接挂载 credentials Secret。
- [ ] 应用、Service 和 probe 端口均固定为 `8317`。
- [ ] values 中没有应用端口配置项。
- [ ] OAuth JSON 只写入 auth PVC。
- [ ] 默认 StorageClass、自定义 StorageClass 和 existingClaim 均可用。
- [ ] 首次登录通过单条 `kubectl exec -it` 命令启动。
- [ ] `using_api` 使用 `jq` 幂等更新。
- [ ] Pod 重启后无需重新 device login。
- [ ] 默认单副本和 `Recreate` 生效。
- [ ] Ingress 模板结构与 `sub2api` 一致。
- [ ] 默认 Ingress class 面向 Traefik。
- [ ] chart 不创建证书或 TLS Secret。
- [ ] Traefik TLS 和 annotations 可完全由用户配置。
- [ ] bitnami common 覆盖通用命名、标签、镜像、资源、亲和性和 Ingress backend。
- [ ] 无 API Key 请求返回 `401`，有 Key 的 `/v1/models` 正常。
- [ ] `grok-4.5` 最小请求成功。
- [ ] 容器、脚本、Helm 和 forgekit 检查通过。

## 21. 实施阶段必须验证

以下项目不是可行性阻塞项，但必须在实现时验证：

1. `v7.2.80` 配置加载器接受 renderer 生成的 JSON 格式 `config.yaml`。
2. 上游镜像以 UID/GID `1001` 运行主服务、init renderer 和 device login 的兼容性。
3. Debian 基础镜像安装 `jq` 后，amd64/arm64 派生镜像均能构建。
4. 主服务 watcher 能否即时加载新建的 xAI auth 文件；如果不能，登录脚本只提示操作者 rollout restart，不给 Pod 增加 Kubernetes API 权限。
5. 当前固定版本是否仍需要 `using_api: true`，升级上游版本前必须重测。
6. 用户 Traefik 环境所需的 entryPoint、certificate resolver、Middleware 或 ServersTransport annotations/CRD。
7. credentials Secret 轮换与用户现有 Secret controller/reloader 的联动方式。

## 22. 最终建议

按本方案实现第一版 chart：

- 维护一个基于上游固定版本的轻量派生镜像。
- 在镜像中内置配置 renderer 和 xAI 登录脚本。
- Secret 只保存 API Key，普通配置全部由 values 控制。
- init container 合成完整配置，主容器只读取生成结果。
- OAuth token 使用 PVC 持久化并保持单写者。
- 首次 device login 由人工显式触发。
- 应用端口固定为 `8317`。
- 对外访问和 TLS 完全交给用户现有 Traefik Ingress。
- chart 结构充分复用 bitnami common，并对齐 `sshpiper`、`codespace` 和 `sub2api` 的仓库模式。
