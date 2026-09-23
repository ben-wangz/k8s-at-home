# 04. 上游 Registry Mirror

## 1. 上游身份和传输 endpoint

请求路径中的第一段是逻辑 upstream authority。它参与 repository、cache key、task identity 和日志关联，但不等同于某一次 HTTP 传输 endpoint。

- docker.io/library/busybox 的逻辑 authority 是 docker.io，repository 是 library/busybox。
- ghcr.io/org/image 的逻辑 authority 是 ghcr.io，repository 是 org/image。
- host:5000/team/image 的逻辑 authority 是 host:5000，repository 是 team/image。
- authority、repository、reference 和 digest 在整个重定向、重试、缓存和固化任务过程中保持不变。

含 `/` 的 R 首段必须能解析为 hostname、hostname:port、带方括号的 IPv6 host 或带方括号的 IPv6 host:port；端口必须是十进制且在有效范围内，禁止 userinfo、空端口、未加方括号的 IPv6、控制字符和无法构造为 URI authority 的值。没有 `/` 的 R 不解析公网 authority，继续按 local-only 规则处理。网络连接使用规范化后的传输 authority，但逻辑 R 和认证配置键保留原始路由语义。

backend 不使用用户配置的 upstream 列表或 upstream ID。传输层使用 01 中定义的 well-known profile：`docker.io` 的逻辑 authority 默认连接 `registry-1.docker.io`，并按 Docker Hub 的标准 challenge 使用 `auth.docker.io`；该 profile 只允许 docker.io 的凭据用于这两个明确的认证 host，不扩展为任意跨 host 凭据转发。这不改变逻辑仓库名，也不能成为另一套路由命名空间。其他 authority 默认连接自身的 HTTPS endpoint。初始 endpoint 选定后，上游返回的 HTTP 3xx 只表示传输 endpoint 变化。

默认使用 HTTPS 并校验证书。上游使用 HTTP 必须通过独立的显式配置开启，不能只把 endpoint 字符串的协议改成 http。自定义 CA 可以配置；禁止默认跳过证书校验。

## 2. 标准上游请求流程

公网 GET/HEAD 按以下顺序执行：

1. 根据逻辑 authority 和 repository 生成标准 Registry V2 endpoint；Host header 设置为实际传输 endpoint 的 authority，不把路径中的逻辑 authority 当成 Host，也不转发客户端 Host/Forwarded header。
2. 建立 HTTPS/显式 HTTP 连接，执行 DNS、IP、端口和代理安全检查；Registry object 请求默认使用 identity content coding，不能让 HTTP client 自动解压或重新压缩 bytes。
3. 处理 /v2/、manifest/index、blob、Range 和 token challenge。
4. 校验响应状态、Content-Type、Content-Length（如果存在）、声明 size 和实际内容 digest；`Docker-Content-Digest` 作为附加校验处理，不能替代 digest-addressed 请求中的本地 digest。manifest 的 canonical digest 可能受 media type 影响，且声明的 manifest `mediaType` 应与 Content-Type 一致（忽略参数）。
5. 只有 complete 对象写入临时 storage 后，才允许响应需要完整 body 的客户端请求。
6. 公网响应只读；不向上游执行 PUT、POST、PATCH、DELETE、blob mount 或 tag 修改。

上游 endpoint 不可用、认证失败、协议格式错误、SSRF 检查失败或内容校验失败时，不能伪造临时缓存、内置 Registry 命中或 solidification 成功。

按 tag 获取 manifest 时，backend 使用返回 media type 对原始 bytes 计算的 canonical digest 建立对象身份；如果 response 带有 `Docker-Content-Digest`，必须验证其语法，并在同一表示/算法下验证它与返回 manifest 相符，但不能盲信 header。按 digest 获取时始终校验返回内容是否匹配请求 digest；header 即使与请求 digest 不同，也不能替代请求 digest 的校验。该规则遵循 Distribution 对不同 digest domain/media type 的兼容语义，同时保留 content-addressed 请求的安全性。

完整 `200` blob GET 必须验证整个 body 的 digest 和 size。只有客户端明确请求 Range 时才接受 `206 Partial Content`；`206` 只能根据 `Content-Range`、响应长度、声明总大小和请求范围验证本次片段。片段不能单独标记为 complete，也不能被当成完整 digest 内容，只有合并得到完整对象后才能提交 complete。HEAD 没有 body，不能证明内容 digest，只能验证状态和可用的 digest/size headers，且不写 cache complete；如果上游 HEAD 成功却缺少 Registry 所需的关键元数据，backend 返回上游协议错误，不自动改用 GET。

## 3. 上游认证

认证域与客户端和内置 Distribution 完全隔离：

- 客户端 Authorization、Cookie、Proxy-Authorization、Host 和 Forwarded headers 永不转发给公网 Registry；
- 没有该 authority 的上游凭据时，只允许 anonymous pull；
- 需要私有镜像时，必须为每一个逻辑 upstream authority 单独配置 Basic 或 Bearer 凭据；
- 上游凭据只能在内存中使用，不能写入 Helm values、日志、任务状态、cache metadata 或响应；
- challenge 的 token scope 只能请求 pull 所需 scope；
- Bearer challenge 的 realm、service、scope 必须经过 allowlist 和 SSRF 检查；
- challenge 最多触发一次认证重试，不能因循环 challenge 无限请求；
- 重定向到另一 authority 时，不携带原 authority 的 Basic/Bearer；目标 authority 只能使用自己的认证配置，Docker Hub well-known profile 的 `registry-1.docker.io`/`auth.docker.io` 例外必须严格限于该 profile 的 allowlist；
- token endpoint 跨 authority 时，必须命中明确的 origin/path allowlist，并且不能把源 authority 的凭据发送给未授权目标。

详细的配置格式、凭据注入和错误映射见 06-auth-security-and-limits.md。

## 4. HTTP 代理

支持通过代理访问公网 Registry，但不把客户端代理设置转发给上游。代理配置分为：

- 对所有公网 Registry 生效的 backend 默认代理；
- 按 authority 单独配置的代理，优先于 backend 默认代理；
- 未选择任何代理时的直接连接。

HTTP_PROXY、HTTPS_PROXY 和 NO_PROXY 不做 registry-hub 专用解析，而是按 Linux 程序的全局环境变量规则影响整个 Go 进程。有效代理选择遵守以下规则：`NO_PROXY` 命中公网 authority 时直连；否则按 authority 专属应用代理、全局应用代理和环境代理规则选择代理；没有任何代理配置时才直连。代理一旦被选定，连接失败、超时、代理返回错误或重试耗尽都不得自动改走直连；只有新的请求明确命中 `NO_PROXY` 或有效代理配置发生变化时才能改变连接方式。单独配置的代理凭据与上游 Registry 凭据隔离，不能复用客户端 Authorization。

代理只支持配置声明的 HTTP/HTTPS proxy，不默认支持 SOCKS。即使使用代理，SSRF 检查也必须针对最终目标 authority、解析地址、重定向目标和 token endpoint 执行；不能因为请求经过代理就认为目标安全。

## 5. 重定向和 SSRF

### 5.1 Canonical redirect

重定向目标只改变传输 endpoint，不改变逻辑 upstream、repository、cache key、task ID 或固化根：

- 公网上游的只读请求允许重定向到不同 host，例如 CDN 或独立 blob 存储服务；跨 host 是传输层行为，不产生新的逻辑 upstream，也不改变 repository 或 reference；
- 每一跳重新检查 scheme、authority、port、TLS、DNS、IP 和 SSRF policy；
- 默认禁止 HTTPS 降级到 HTTP；HTTP 目标只有在显式允许时才可访问；
- 只允许 HTTP/HTTPS，不允许 file、unix、data 或其他 scheme；
- 限制最大重定向跳数，并检测循环；
- 跨 host 不携带原始 host 的 Authorization、Cookie 或 proxy credential；
- 目标 host 的认证只能从该目标的独立配置获得；
- 重定向目标的 URL 只在 backend 内部使用，不能暴露给客户端；
- 安全失败、循环、超跳数或不可用目标返回映射后的 502/认证错误，不写 cache complete，不创建或推进固化根。

### 5.2 SSRF 防护

第一段路径可能是任意 host，因此必须至少阻止：

- loopback、0.0.0.0、未指定地址和 IPv4/IPv6 link-local；
- RFC1918、RFC6598、ULA、IPv4-mapped IPv6 和其他私有/保留地址；
- metadata service、Unix socket 和非允许端口；
- DNS 返回多个地址时，只要任一地址不满足 policy，默认拒绝整个 authority；
- 连接建立后再次确认实际 peer IP，防止 DNS rebinding；
- 重定向和 token realm 目标重新执行同样的检查；
- 不允许通过 NO_PROXY、HTTP proxy 或自定义 Host header 绕过检查。

解析器、dialer、连接池和缓存必须绑定经过校验的 authority/IP 结果，不能只校验首次 DNS 查询。后续发现新的攻击面时，在本模块追加规则并同步安全验收。

## 6. Mirror 缓存身份

- 完整对象按 algorithm:digest 保存；只有通过请求/descriptor digest、声明 size 和结构校验的对象可以标记 complete。`Docker-Content-Digest` 不能单独证明对象身份。
- digest 请求可以复用 complete 对象；tag 到 digest 的映射只在当前请求/in-flight 去重窗口中存在，不持久化公网 tag 映射。
- 不同 Accept 变体的 manifest/index 响应按实际 digest + 已验证 mediaType 独立记录；相同 digest 的 data 可以去重，但 representation 元数据不能互相覆盖，原始 bytes 不改写。
- ETag、Last-Modified 和上游缓存时间不能作为内容身份，不能替代 digest 校验。
- 本地 Distribution 命中后不访问公网 refresh，不合并公网结果。
- 错误响应、部分 body、digest mismatch、无效 manifest、SSRF 失败和不可接受 media type 不写错误缓存。
- 完成固化并经 Distribution 标准 API 读取验证后，cache 才按 storage 生命周期清理；清理失败不影响已成功的本地内容。

manifest/index 的 discovery、兼容交付和多架构根规则由 01 和 02 定义。上游 mirror 必须保留交付对象的原始 bytes、media type、headers 中有意义的 Registry 信息和 descriptor 扩展字段。

公网 Referrers discovery 遵守 OCI 的 native/fallback 顺序：native endpoint 的 2xx（包括空 index）直接作为本次结果；只有 404 才读取同一 repository 的 digest-derived fallback tag；其他状态、格式错误或 fallback 读取失败不继续回退。registry-hub 的分页参数在 backend 侧处理；需要跟随上游 Link 时必须重新执行认证、代理、TLS、SSRF 和重定向检查，并用 backend 的不透明 Link/cursor 返回，不能暴露上游 URL 或凭据。公网如果没有应用 `artifactType` 过滤，backend 必须在完整收集并分页前执行过滤，只有实际过滤后才能设置 `OCI-Filters-Applied: artifactType`。公网 fallback 只读，不更新公网 tag，不自动把发现到的 referrer 下载、缓存或固化；具体 tag 计算规则与本地保护规则由 01-routing-and-registry-api.md 定义。

## 7. Blob 下载和断点

- 使用标准上游 /v2/<repository>/blobs/<digest>，不使用 descriptor.urls。
- 支持 HEAD、GET、Range 和大对象流式下载，使用有界 buffer；206 片段遵守本节的 partial-object 校验，不能伪装成完整 digest 对象。
- 断点 checkpoint 绑定 authority、repository、digest、声明 size、已验证 offset、响应身份和 fencing token；不能跨对象复用。
- Range 合并后必须重新验证完整 digest/size；不能把未验证分片标记为 complete。
- 客户端 HEAD 不触发 GET、cache 写入或 solidification 任务。
- 上游 HEAD 405 不自动 GET；根据客户端请求返回标准错误。

## 8. 错误和重试

前台 GET/HEAD/token 请求总计最多 3 次，只重试网络中断、408、429、502、503 和 504。默认指数退避为 1、2、4 秒，总等待上限 15 秒；合法 Retry-After 单次最多等待 60 秒并受整体 deadline 约束。其他 5xx 默认按终态处理，遵守 Distribution API 对 502/503/504 临时错误与其他 5xx 的区分。

不自动重试：

- 404；
- 401/403 等认证或权限拒绝；
- digest mismatch；
- SSRF、TLS、scheme 或重定向安全失败；
- manifest/index 格式错误；
- 客户端没有可接受 media type；
- 明确的客户端 4xx。

前台重试耗尽后直接返回映射错误，不等待后台任务。后台固化独立持久化 retryCount 和 nextAttemptAt，并复用已经验证的 checkpoint；后台暂时性失败进入 retry_wait，永久校验/安全错误进入 failed。前台和后台的重试预算、日志和状态相互独立。

推荐错误映射：

| 上游情况 | backend 行为 |
| --- | --- |
| 404 | 作为公网未找到返回，不创建错误缓存 |
| 401/403 | 返回认证/权限错误，不把客户端凭据转发或升级权限；上游 `WWW-Authenticate` 不直接透传为客户端 challenge |
| 408/429/502/503/504 | 按预算重试，耗尽后返回上游错误并保留可恢复 checkpoint |
| 其他 5xx | 默认终态返回，不写错误缓存；后台只有在明确配置的恢复策略下重新调度 |
| TLS/SSRF/redirect 安全失败 | 502 或安全错误，不缓存 |
| digest/size mismatch | 502/内容损坏，隔离对象，不重试 |
| 不可接受 media type | 406，不缓存、不创建固化任务 |
| storage 故障 | warning、fail-closed，不访问公网或发布根 |

## 9. 并发和请求取消

同一 authority、repository、digest 的公网下载使用 02 中定义的对象 lease。leader 续租、写 checkpoint 和提交 complete；waiter 等待 complete 或最终错误，最多等待 30 秒，不重复下载。

客户端断开、请求超时或服务关闭时，必须取消当前响应的 context，但不能删除仍被其他任务或 waiter 引用的 complete/staging 对象。后台任务的 checkpoint 由 storage 保留并可恢复。
