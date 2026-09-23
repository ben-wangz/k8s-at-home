# 06. 认证、安全与资源限制

## 1. 三个隔离的认证域

registry-hub 必须把以下认证域完全隔离：

1. 客户端到 backend；
2. backend 到内置 Distribution；
3. backend 到公网 Registry/token endpoint。

任何域的 Authorization、Basic password、Bearer token、proxy credential、Cookie 或 TLS client credential 都不得自动复制到另一个域。

## 2. 客户端和管理面认证

### 2.1 客户端

客户端支持与 Distribution 类似的两种模式：

- 未配置客户端认证时按 no-auth 运行；
- 通过 Secret 或等价注入的静态 Basic/Bearer 凭据。

未配置客户端认证时按 no-auth 运行；这是可信网络（通常为局域网）中的有意配置。backend 启动时如果没有配置客户端认证，必须写 warning 日志提醒用户；这不是启动失败条件。认证配置已存在但格式错误或无法读取时，不能静默降级为 no-auth，而必须使启动或 readiness 失败。no-auth 模式不执行客户端身份校验，允许 Registry API 操作，但仍受 GC 写入闸门、速率、并发、大小和安全限制约束。客户端凭据只用于校验入站请求，不用于公网回源。

启用静态 Basic/Bearer 客户端认证时，权限至少拆分为 pull、push、delete、task-admin、referrer-prune 和 gc-admin，默认 deny。无权限请求返回标准 401/403，不回源，不把认证失败解释为 local miss。若 no-auth 在实现内部表示为 anonymous principal，该 principal 必须显式绑定为允许上述 Registry 操作的可信网络角色，不能套用 default deny。

### 2.2 管理面

管理面使用独立的内部 listener/address 和独立的静态 Basic/Bearer 凭据。管理面不得复用客户端凭据，也不提供账号注册、密码修改或 Token 签发。

至少提供以下受保护权限：

- 查看任务、固化状态、GC 状态和健康细节；
- retry/cancel/cleanup 任务；
- referrer reconcile；
- GC trigger/retry/recover；
- referrer prune/replace。

管理接口的身份、凭据、token、完整 URL 和上游 header 不得写入日志或任务状态。

## 3. 内置 Distribution 认证和 TLS

Distribution 只允许 backend 访问。backend 到 Distribution 使用独立的内部认证配置和 private HTTPS Service：

- Distribution 使用自签内部服务端证书；
- backend 默认启用证书校验，并通过 Helm chart 或 Secret 注入该证书的信任 CA；
- 可以配置自定义 CA，但禁止默认跳过证书校验；
- backend 不提供自己的面向客户端 CA 机制，不做 client certificate/mTLS；
- 外部客户端 TLS 由 Ingress 负责；
- internal Distribution 的 Basic/Bearer credential 不得作为客户端 credential 或上游 credential。

Distribution 的地址、证书、认证和连接池参数通过环境变量或 Secret 配置。backend 启动时应验证证书、服务名、认证配置和 Registry /v2/ 可达性。

backend 到 Distribution 的请求若收到 `401 Unauthorized`，只能依据内部配置的 challenge/credential 做一次内部重试；realm、service 和目标 authority 必须匹配内部配置。重试仍失败或收到 `403` 时，backend 返回不暴露内部 challenge 的 502/503 标准错误。内部 challenge、Basic/Bearer 和 token 不得出现在客户端响应，也不得进入上游请求。

## 4. 上游认证

默认不配置上游 credential 时，只允许拉取公网 anonymous 镜像。需要私有镜像时，必须对每个逻辑公网 Registry authority 独立配置 Basic 或 Bearer 凭据：

- 客户端 Authorization 永不转发给公网；
- Basic 只发送到其绑定的 authority；
- Bearer token 只用于声明的 pull scope；
- challenge 只允许一次重试，realm/service/scope 必须经过安全校验；
- redirect 或 token realm 跨 host 时，不带源 host 凭据，目标 host 只有自身明确配置才能认证；
- 上游 credential 只在内存中使用，不写任务 state、cache metadata、日志或错误 body。

上游认证、redirect、token endpoint、代理和 SSRF 的完整传输规则见 04-upstream-mirror.md。

## 5. 代理和凭据注入

HTTP_PROXY、HTTPS_PROXY 和 NO_PROXY 是影响整个 Go 进程的标准 Linux 环境变量。公网 Registry 还可以配置一个全局应用代理以及按 authority 的单独代理；单独配置优先于应用默认配置，环境变量可以覆盖最终代理选择，NO_PROXY 可以让匹配目标直连。代理选定后不得因失败自动直连；没有代理配置或命中 NO_PROXY 时才允许直连。

代理 credential 必须独立保存，不能复用客户端或 Registry credential。支持 HTTP/HTTPS proxy，不默认支持 SOCKS。代理无法绕过 TLS、SSRF、重定向和目标 authority 校验。

backend 配置从环境变量读取。建议的配置分组：

| 配置 | 用途 |
| --- | --- |
| REGISTRY_HUB_CLIENT_ADDR | 外部 Registry listener |
| REGISTRY_HUB_ADMIN_ADDR | 管理 listener |
| REGISTRY_HUB_CLIENT_AUTH_CONFIG | 客户端认证配置引用 |
| REGISTRY_HUB_ADMIN_AUTH_CONFIG | 管理面认证配置引用 |
| REGISTRY_HUB_DISTRIBUTION_ENDPOINT | 内置 Distribution 私有 Service |
| REGISTRY_HUB_DISTRIBUTION_AUTH_CONFIG | backend 到 Distribution 的认证引用 |
| REGISTRY_HUB_DISTRIBUTION_CA | 内部自签 CA 引用 |
| REGISTRY_HUB_UPSTREAM_AUTH_CONFIG | 按公网 authority 的上游认证配置 |
| REGISTRY_HUB_UPSTREAM_TRANSPORT_CONFIG | 按公网 authority 的 HTTPS/显式 HTTP、CA 和 endpoint 安全策略 |
| REGISTRY_HUB_PROXY_CONFIG | 全局/按 authority 的公网代理配置 |
| REGISTRY_HUB_STORAGE_BACKEND | filesystem 或 s3 |
| REGISTRY_HUB_FILESYSTEM_ROOT | FileSystem storage 根路径 |
| REGISTRY_HUB_S3_* | S3 endpoint、bucket、prefix、TLS 和凭据引用 |
| REGISTRY_HUB_GC_EXECUTABLE | 单机模式下 Distribution GC executable 的绝对路径；Kubernetes 模式由 Job 镜像提供 |
| REGISTRY_HUB_GC_CONFIG | 单机模式下与 Distribution storage 一致的 GC 配置引用；Kubernetes 模式由 Job 配置提供 |
| REGISTRY_HUB_DISTRIBUTION_LIFECYCLE_EXECUTABLE | 可选的单机生命周期适配器绝对路径，用于启用 Distribution 只读/停止纵深防御 |
| REGISTRY_HUB_DISTRIBUTION_LIFECYCLE_CONFIG | 可选的 Distribution 只读/停止及恢复可写状态的受控适配配置；未配置时由 backend 闸门和唯一写入口检查保证 GC 前提 |

Secret 或 workload identity 负责注入真实凭据。凭据不能写入 Helm values、容器 args、普通环境变量明文、日志、任务状态、cache metadata 或 metrics label。

## 6. SSRF 和网络安全

第一段路径可能由客户端任意指定。必须阻止 loopback、未指定地址、私有/保留 IPv4/IPv6、IPv4-mapped IPv6、link-local、metadata service、Unix socket 和未允许端口。DNS 多地址、连接后的 peer IP、redirect、token realm 和 proxy 目标都重新执行检查。

不能通过 NO_PROXY、proxy、Host header、HTTP 302 或 DNS rebinding 绕过 SSRF policy。详细规则和未来增补统一维护在 04-upstream-mirror.md。

## 7. 资源限制默认值

以下默认值用于防止恶意或意外的大对象、递归图和并发请求耗尽资源：

| 类别 | 默认限制 |
| --- | ---: |
| 单个 manifest/index | 16 MiB |
| 单次 upload request | 256 MiB |
| 单个 blob | 100 GiB |
| 单根闭包对象数 | 10,000 |
| 单根闭包总大小 | 1 TiB |
| upload idle timeout | 30 分钟 |
| upload maximum lifetime | 24 小时 |
| client total concurrency | 128 |
| no-auth source concurrency | 16 |
| local push concurrency | 16 |
| per-upstream concurrency | 8 |
| all-upstream concurrency | 32 |
| active solidification workers | 4 |
| pending persisted tasks | 1024 |
| global request rate | 200/s，burst 400 |
| no-auth source rate | 20/s，burst 40 |

还必须限制最大 manifest/index 递归深度、descriptor 数量、descriptor.data 大小、header/body 读取、Range 数量、redirect 跳数、token 响应大小和单任务 deadline。具体数值作为配置项，但不能取消 digest、size、结构和 SSRF 校验。

超过 body、对象、闭包或并发限制时返回标准 413、429 或 503，并根据是否可重试附带 Retry-After。拒绝的请求不写 complete、不创建不可恢复任务、不访问公网以绕过限制。

_catalog 默认关闭；启用时必须单独设置权限、分页上限和速率限制，不能把公网目录或 storage List 作为数据源。

## 8. 安全日志和错误

日志使用结构化 JSON。必须记录足以排障的 request ID、逻辑 authority、repository、digest、错误分类、重试次数、任务 ID 和 GC 状态，但不得记录：

- Basic password、Bearer token、proxy credential 或 Secret；
- 客户端完整 Authorization；
- token realm 中的敏感 query；
- 带 credential 的 URL；
- 完整 manifest 中可能包含的敏感 annotation；
- S3 access key 或 workload identity token。

认证失败、SSRF、TLS、storage、digest mismatch 和 GC unknown 必须有稳定低基数错误分类。响应 body 不能回显上游私密错误或凭据。
