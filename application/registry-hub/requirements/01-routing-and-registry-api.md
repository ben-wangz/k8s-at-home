# 01. 路由与 Registry API

## 1. 协议范围

backend 对外提供标准 Registry V2/OCI Distribution HTTP API，至少覆盖：

- /v2/ version check 和标准 challenge 响应；
- manifest/index 的 GET、HEAD、PUT、DELETE；
- blob 的 GET、HEAD、Range 和标准 DELETE；DELETE 仍受本地认证、generation 和 GC 闸门约束；
- blob upload 的创建、状态查询、分块、完成和取消；
- tags/list；
- _catalog（默认关闭，启用时仍只读取内置 Registry）；
- /referrers/<subject-digest> 以及 OCI 规定的 fallback 行为。

backend 只对外暴露统一入口。客户端不会直接访问 Distribution Service。

`GET /v2/` 始终由 backend 本地处理，不回源：客户端认证成功或启用 no-auth 时返回
`200 OK`；认证缺失或失败时返回 `401 Unauthorized`、`WWW-Authenticate` 和标准
`Docker-Distribution-API-Version: registry/2.0`。backend 不把 Distribution 的内部
challenge、Service 地址或内部凭据暴露给客户端。backend 生成的 Registry 错误使用
`{"errors":[{"code":"...","message":"...","detail":{}}]}` JSON envelope，不返回 HTML，也不直接
回显公网 Registry 的私密错误 body；未知错误 code 由客户端按 `UNKNOWN` 处理。
至少正确处理 `BLOB_UNKNOWN`、`BLOB_UPLOAD_INVALID`、`BLOB_UPLOAD_UNKNOWN`、
`DIGEST_INVALID`、`MANIFEST_BLOB_UNKNOWN`、`MANIFEST_INVALID`、`MANIFEST_UNKNOWN`、`NAME_INVALID`、
`NAME_UNKNOWN`、`PAGINATION_NUMBER_INVALID`、`RANGE_INVALID`、`SIZE_INVALID`、
`MANIFEST_UNVERIFIED`、`TAG_INVALID`、`TOOMANYREQUESTS`、`UNAUTHORIZED`、`DENIED` 和 `UNSUPPORTED`，并把错误 detail 中的内部
I(R)、内部 Location 和私有 Service 信息改写或移除。

## 2. 路径解析

### 2.1 Endpoint 识别

backend 只接受规范 /v2/ 路径。普通仓库请求必须从右侧匹配明确 endpoint：

| Endpoint | 说明 |
| --- | --- |
| /manifests/<reference> | reference 是合法 tag 或 digest |
| /blobs/<digest> | digest 使用合法算法和编码 |
| /blobs/uploads/ | 创建 upload session、single-request monolithic upload 或尝试 cross-repository blob mount |
| /blobs/uploads/<uuid> | session 状态、分块、完成和取消 |
| /tags/list | 只允许 GET 和受限分页参数 |
| /_catalog | 只允许 GET 和受限分页参数，不带仓库名 |
| /referrers/<digest> | subject digest 的 referrer 发现 |

除 `/v2/` 和 `/_catalog` 外，endpoint 前面的全部路径是外部逻辑仓库名 R。不能通过首段/末段简单拆分，也不能因为仓库名包含 manifests、blobs、tags 或 referrers 就误判 endpoint。R 的首段可以是包含 `:` 的逻辑 upstream-host；因此 R 不直接套用内置 Distribution 的仓库名语法，而是在解析后映射为内部名称 `I(R)`。

URL path 只解码一次。编码后的 /、反斜杠、. 和 .. 段、空段、重复编码、非法控制字符和会改变 endpoint 的规范化请求必须拒绝。query 不属于 R，只能按对应 endpoint 的允许参数解析：upload 创建请求允许 `mount`、`from`、`digest` 和 `digest-algorithm` 的标准组合，upload 完成请求允许重复的 `digest` 参数；manifest digest PUT 允许重复的 `tag` 参数；tags/catalog 允许 `n`、`last`；Referrers 标准查询允许 `artifactType`，registry-hub 额外提供的 `n`、`last` 分页参数只由 backend 解释，不原样转发给 native Distribution；其他 query 默认拒绝。

reference 必须是合法 tag 或 digest：tag 最长 128 字符并匹配 `[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}`，digest 使用合法 algorithm 和十六进制编码。backend 不做 Docker Hub short-name 展开，不把 library 当作特殊名称。

### 2.2 upstream-host 规则

R 的路由规则不区分 mirror 命名空间和 local 命名空间，也不读取 upstream 配置：

| 请求路径 | 本地查询 | 本地 404 后的公网解析 |
| --- | --- | --- |
| busybox | busybox | 不回源，返回未命中 |
| docker.io/library/busybox | docker.io/library/busybox | authority=docker.io，repository=library/busybox |
| library/busybox | library/busybox | authority=library，repository=busybox |
| host:5000/team/image | host:5000/team/image | authority=host:5000，repository=team/image |

第一段始终是外部逻辑 authority，不要求预先登记，也不会因此产生 upstream ID 或新的命名空间。传输层允许使用内置的 well-known transport profile：逻辑 authority 恰为 `docker.io` 时，默认请求官方 Registry endpoint `https://registry-1.docker.io`，并允许其标准 `auth.docker.io` Bearer challenge；该 profile 将这两个明确列出的 host 视为同一个 `docker.io` 认证流程，docker.io 的上游凭据只有在对应 challenge 明确需要时才能发送到这两个 host，不能发送到任意其他重定向目标。这只是传输 endpoint 别名，R、认证配置键、缓存身份和任务身份仍使用 `docker.io`。不能把这个别名扩展成用户可配置的 upstream 列表，也不能假设 `docker.io` 一定通过 302 自动转移。其他 authority 默认使用 `https://<authority>`，显式 HTTP 和其他 HTTP 3xx 只改变传输 endpoint，重定向处理规则见 04-upstream-mirror.md。

### 2.3 内部 Distribution 仓库名映射

当前 Distribution `reference.NameRegexp` 允许可选的 `domain:port/` 前缀；因此不能简单地
说 Distribution 的所有 repository name 都禁止 `:`。但这里的首段 A 是 registry-hub 的
逻辑 upstream-host，不应被内置 Distribution 当成 domain 重新解释。backend 因此把 I(R)
限定为不带 domain 前缀的 ordinary remote-name：每个 component 使用
`[a-z0-9]+(?:(?:[._]|__|[-]+)[a-z0-9]+)*`，总长度不超过 255；并使用以下只存在于内部
的可逆映射：

- 当 R 含 `/` 时，只映射首段 A，后续 repository component 保持原样；只有当 A 完全匹配 ordinary component 语法、不以 `e` 开头、不是 `localhost` 且不包含 `.`/`:` 时才保持原样。其他 A 按 UTF-8 bytes 编码为 `e` 加上逐字节结果：只有 `[a-z0-9]` 保持原字符，其他 byte 编码为 `_` 加两个小写十六进制字符。这样会同时隔离 `docker.io` 这类会被 Distribution 当作 domain 的首段、`host:port`、IPv6、`localhost` 和保留的 `e` 前缀，编码结果仍符合 Distribution ordinary remote-name 语法。
- 当 R 不含 `/` 时，R 是没有 upstream-host 的本地仓库，I(R) 直接等于 R；如果它不符合 ordinary component 语法则返回 `NAME_INVALID`，不回源。
- 例如 `docker.io/library/busybox` 映射为 `edocker_2eio/library/busybox`，`library/busybox` 保持同名内部仓库，`host:5000/team/image` 映射为 `ehost_3a5000/team/image`，`[2001:db8::1]:5000/team/image` 映射为 `e_5b2001_3adb8_3a_3a1_5d_3a5000/team/image`。
- `e` 是 backend 保留的编码前缀：参与映射的未编码首段不会以 `e` 开头，所有编码结果都以 `e` 开头并保留完整 byte escape，因此可无歧义逆向解析。不引入 `mirror/`、`local/` 或其他业务命名空间。外部请求、Location、任务身份、缓存身份和日志使用逻辑 R，只有发往 Distribution 的路径使用 I(R)。
- I(R) 在发给 Distribution 前必须再次校验 ordinary component 语法和总长度；超限或非法输入返回标准 `NAME_INVALID`，不能让 Distribution 收到未校验路径。

所有 local-first 查询、写入、删除、tag/fallback index 维护和固化导入都对同一个 I(R) 操作。该映射同时隔离 Distribution 的名称语法和 domain 解析语义，不改变外部路由规则，也不使 mirror 和 local 产生两个命名空间。

## 3. 读取路由

GET/HEAD 以及其他只读查询按以下顺序处理：

`/v2/`、`/_catalog` 和 upload session（包括状态查询与取消）始终只访问内置 Distribution；它们的 404 不触发公网回源。普通 manifest/blob GET/HEAD 和各自定义的 tags/list、Referrers 查询按以下顺序处理：

1. 向内置 Distribution 请求 I(R)、reference 和经过 allowlist 的 Registry headers/query；backend 生成指向内部 Service 的 `Host`，不转发客户端的 `Host`、`Forwarded`、`X-Forwarded-Host`、`Authorization`、`Proxy-Authorization`、Cookie 或外部 challenge，backend 使用独立内部凭据。请求和响应不能被 HTTP client 自动解压/重新压缩而改变 Registry object bytes；hop-by-hop headers 由 backend 终止并重新生成。所有对外可见的 path 和 Location 仍使用 R。
2. 内置 Distribution 返回成功响应时，返回其语义结果但先重写内部 I(R)、Location、Link 和可见名称，并移除内部 challenge，不访问公网。
3. 只有 endpoint 语义明确表示对象/仓库不存在的 HTTP 404 才算 local miss，例如 manifest/blob 的 `MANIFEST_UNKNOWN`、`NAME_UNKNOWN` 或 `BLOB_UNKNOWN`；如果 404 可能来自 Accept 协商、未支持能力、解析或配置错误，backend 必须先做一次 local-only 的足够宽的能力/Accept 探测再决定是否回源。其他 404 不能被当作公网 miss。
4. 内置返回 401 或 403 时视为 backend 到 Distribution 的内部认证/授权故障，返回不暴露内部 challenge 的 502/503 标准错误；405、429、5xx 或其他状态按对应 Registry 错误映射，不回源。
5. R 不含 / 时，local miss 直接返回失败；含 / 时才按第一段 authority 访问公网。

本地命中不区分是本地 push 还是既往 mirror 固化。内置 Registry 的结果是当前内容的权威来源，不做公网刷新和结果合并。

## 4. 写入路由

POST、PATCH、PUT、DELETE 不能转发到上游，只能操作内置 Distribution：

- PUT manifest/index、blob upload、upload completion、tag 操作进入内置 Distribution；
- manifest/index PUT 必须验证去除媒体类型参数后的 `Content-Type` 与 body 中声明的 `mediaType` 一致（若 body 声明了该字段），并验证 digest reference 与 Distribution 对该 media type 计算的 manifest digest 一致；成功响应遵守 Distribution 的 `201`、`Location` 和 `Docker-Content-Digest` 语义。原始 manifest/index bytes 不得因校验而重新序列化；
- PUT manifest 使用 digest reference 时，可按 OCI 规范携带一个或多个 `tag` query 参数；backend 可以把该请求拆成同一原始 manifest 的标准 digest PUT 和逐个 tag PUT，不要求内置 Distribution 理解这个可选 query 扩展。这些 tag 与普通 tag reference 一样进入内置 Distribution，并受保护 tag、generation 和 GC 闸门约束；
- DELETE manifest/tag/blob/upload 只进入内置 Distribution，并遵守 05-delete-and-gc.md 的 generation 和 GC 闸门；普通 tag 删除遵循 Distribution/OCI 的启用状态，不能被转换成 digest 删除；
- 上游永远是只读 source；backend 不向上游 push、delete、mount 或修改 tag；
- upload UUID 仅属于 backend 到内置 Distribution 的本地 session，不会发送给公网。

本地 push 与 mirror 固化可以使用相同仓库路径。相同 tag 的不同 digest 竞争规则：先完成根 manifest/index 发布并重新读取验证的一方获胜，后续不同 digest 返回 409 Conflict；相同 digest 的重复发布是幂等成功。digest PUT 携带的每个 `tag` 都适用同一保护和竞争规则，并按 OCI `OCI-Tag` 响应语义报告实际接受的 tag；部分 tag 失败时不得伪造全部 tag 成功，已接受的 tag 必须可由响应和后续读取确认。

标准 cross-repository blob mount (`POST /v2/<name>/blobs/uploads/?mount=<digest>&from=<repository>`)
只在内置 Distribution 内执行。backend 对目标 name 和 `from` repository 分别解析并映射为 I(R)，绝不向公网尝试 mount；mount 失败时保留 Distribution 标准的 `202 Accepted` upload fallback。

## 5. Manifest/index 交付

manifest/index 的公网请求必须执行支持范围内的 Docker Schema 2、manifest list 和 OCI media type Accept 协商：

- discovery 优先请求能代表完整固化根的 index/list；
- discovery 返回的 media type 被客户端接受时，直接交付原始响应；成功响应的 `Content-Type` 应与 manifest/index 自身声明的 `mediaType` 一致（忽略媒体类型参数）；
- 客户端不接受合法 discovery index 时，最多再请求一次客户端可接受的 manifest/index 响应。该响应单独验证和缓存，后台仍以完整 discovery index 及其全部平台固化；
- discovery 根已合法验证但兼容请求失败时，当前请求返回协商或上游错误，后台仍继续固化 discovery 根；
- 没有客户端可接受的类型返回 406 Not Acceptable，不缓存错误、不创建错误固化任务；
- digest 请求只能返回该 digest 对应的原始对象，不以其他 media type、index 或平台对象替代；
- backend 不按 platform 选择、裁剪或重写 index；
- HEAD 只取得 headers，不写临时缓存、不创建任务、不产生 body；成功响应必须提供可验证的 `Docker-Content-Digest` 和 `Content-Length`。上游 HEAD 为 405 时不得自动改用 GET。

`Docker-Content-Digest`、`Content-Type`、`Content-Length`、`ETag`、`Range`、`Docker-Upload-UUID`、`OCI-Chunk-Min-Length`、`OCI-Tag`、`OCI-Subject` 和缓存相关 header 需要按 Registry 语义重写或保留。只要 backend 改写了响应 body、`name`、`Link` 或其他影响表示的字段，就必须重新计算 `Content-Length`，并重新计算或移除不再对应 body 的 `ETag` 和 digest header。digest-addressed 请求始终以请求中的 digest 作为本地校验依据；不能用 response 的 `Docker-Content-Digest` header 替代它。manifest 的 canonical digest 可能受 media type 影响，blob 则必须校验实际 body digest。原始 manifest/index bytes 不得重新序列化。

## 6. Blob、Range 和上传

- blob GET/HEAD 先查询内置 Distribution；local 404 后才允许按同一逻辑从公网标准 blob API 获取。
- 上游 blob 的完整 `200` body 必须验证实际 digest 和声明 size；客户端请求的可满足 Range 返回标准 `206`/`Content-Range`，不可满足的范围返回 `416`，两者都必须验证范围、总大小和传输偏移。`206` 片段不能单独宣称完整对象 digest 已验证，必须在合并完整对象后才能提交 complete。
- 对客户端支持流式响应，但公网对象在返回前必须至少完整写入并验证本次响应所需的临时对象；大对象使用有界 buffer，不把整个 blob 无界加载到内存。
- 上游 descriptor.urls 只保留原始字段，永不访问，也不作为 blob fallback；标准 blob API 缺失时不发布根对象。
- descriptor.data 若存在，按 02-artifacts-and-solidification.md 进行 Base64、size、digest 和资源限制校验。
- 内置 Distribution 返回 BLOB_UNKNOWN 时，backend 不代为从 descriptor URL 下载；本地 push 遵守标准 Registry 错误。
- upload 使用 Distribution 规定的流程：创建返回 `202` 和 `Location`；支持 `POST /blobs/uploads/?digest=<digest>` 的 single-request monolithic upload，若不采用单请求则返回标准 `202` fallback；非 sha256 的分块 upload 可使用 OCI `digest-algorithm`；状态查询返回 `204`，有序 `PATCH` 使用 `Content-Range`，不接受的范围返回 `416` 和当前 `Range`，最终 `PUT ?digest=<digest>` 返回 `201`，取消 upload 成功返回 `204`。已上传全部 chunk 时允许用零字节最终 PUT 完成；完成请求携带多个 `digest` 时必须按 Distribution 的校验/拒绝语义处理。
- 每次响应返回的 `Location` 都是客户端下一次请求使用的 opaque URL；backend 必须把内置 Distribution 返回的内部路径（包括 I(R)）重写为外部 backend URL，不能要求客户端自行拼接或解码。blob 完成、mount 或 GET 返回的绝对 storage Location 也不得泄露给客户端，必须由 backend 在内部跟随或安全代理。upload UUID 不跨公网转发。
- 内置 Distribution 的 blob GET/HEAD 可能返回指向 storage 的 `307`（HTTP/1.1 下可能为 `302`）redirect；backend 必须在内部跟随并安全代理响应，绝不能把私有 storage Location、内部 hostname 或签名 URL 暴露给客户端。启用该能力时的 endpoint allowlist、TLS、SSRF、重定向和凭据边界见 07-operations-deployment-and-observability.md。

## 7. tags/list 和 catalog

GET /v2/<name>/tags/list 先读 I(R) 对应的内置 Distribution。query 仅允许 `n` 和 `last`：`n` 可省略，提供时范围为 0 到 backend 配置的最大页面大小（默认 1000）；`n=0` 返回空 tags 且不带下一页 Link；`last` 必须是非空合法 tag，且可以在省略 `n` 时单独使用，此时 backend 使用配置的页面大小。没有 `n` 和 `last` 时，backend 保留标准的完整列表语义，或在配置了响应上限时使用标准 `Link` 分页，不能无 Link 地静默截断；参数非法返回 400，不访问公网。local 200 是本地权威结果，不与公网 tag 列表合并；local 404 且 R 含 / 时，可以按同一 upstream 规则查询公网 tags/list，并返回重写到 backend 的分页结果：JSON 中的 `name` 使用 R，tags 按 Registry 规定的 lexical/ASCIIbetical 顺序返回，`Link` 使用 backend URL。R 不含 / 时 local 404 直接返回未命中。tags/list 不创建 artifact 固化任务，也不持久化公网 tag 到 digest 映射。

_catalog 默认关闭。启用时只读取内置 Distribution，支持标准 `n`/`last` 分页；请求中的 `last` 使用外部逻辑 repository 名，backend 映射后再查询，响应中的 repository 和 `Link` 都改写为外部 backend 语义，并把可逆的 I(R) 条目反向改写为逻辑 R；只有包含 `/` 且首段符合编码格式的条目才按编码逆向，单段普通仓库名保持为本地逻辑 R。不能把内部编码名泄露给客户端，也不能通过扫描公网或 storage List 构造目录。

## 8. Referrers

backend 对外提供标准 GET /v2/<name>/referrers/<subject-digest>，支持 artifactType 过滤、分页 Link 和 OCI-Filters-Applied 语义。成功结果必须是 `200 OK`、`Content-Type: application/vnd.oci.image.index.v1+json` 的 OCI image index；没有匹配项时返回空 `manifests` 数组；每个 descriptor 必须保留 digest、size、mediaType、artifactType（manifest/index 按 OCI 规则推导或省略）和 annotations，不能把 fallback index 或上游响应原样暴露为内部名称：

1. 先通过标准 Registry API 请求 I(R) 对应的内置 Distribution 原生 endpoint。
2. 合法原生 2xx 响应（包括空结果）是本地 referrer 结果的权威来源，不与 fallback index 合并。
3. 原生 endpoint 只有 404 时，才通过标准 manifest API 读取 OCI 规定的 digest-derived fallback tag。
4. 原生返回其他错误、格式非法或读取 fallback 出错时，返回内部 Registry 错误，不回源、不把错误当作 local miss。
5. fallback tag 缺失代表本地空结果；不做全量 bootstrap，也不扫描 _catalog、tags 或 Distribution 私有存储。若 R 含 /，此时可以按同一 upstream、subject 和 artifactType 查询公网 Referrers；registry-hub 自己的 `n`、`last` 和 cursor 在 backend 侧解释，上游分页通过受限跟随或不透明 cursor 处理，不能把公网 Link 原样暴露给客户端。公网 Referrers 响应只作为本次发现结果返回，不自动下载、缓存或固化其中的 referrer；如果公网未应用 `artifactType` 过滤，backend 必须在完整收集并分页前应用过滤，且只有实际过滤后才能返回 `OCI-Filters-Applied: artifactType`。R 不含 / 时直接返回本地空结果。
6. fallback index 的 descriptor 按 referrer digest 去重并稳定升序排列；artifactType 过滤先于分页，Link 必须重写到 backend，并使用绑定 subject、过滤条件、index digest 和最后 digest 的签名/完整性游标。index digest 变化或游标校验失败返回 409，不能用新 index 解释旧游标。原生 Distribution 或公网返回的 referrers 如果没有声明已应用过滤，backend 也必须执行同样的过滤和分页顺序。
7. native endpoint 的能力动态识别，不作为启动/readiness 依赖；能力 cache 只存在进程内存，不缓存查询内容。只有在目标 repository 已知存在时，原生 404 才能作为该 repository 的 unsupported 证据并最多缓存 30 秒；repository 本身不存在时的 404 不能污染全局能力判断。其他错误或格式非法不缓存，并立即使已有能力缓存失效。

fallback index 只通过标准 manifest GET/PUT/DELETE 维护：

- backend 根据 native capability cache/probe 选择维护路径。原生 endpoint 返回 2xx 时，subject manifest/index push/delete 只通过标准 Distribution manifest API 执行，原生 endpoint 是唯一权威来源；push 成功必须得到 `OCI-Subject: <subject digest>`，缺失该 header 视为 Distribution 协议错误，不悄悄切换到 fallback。
- 只有原生 endpoint 返回 404 时，subject push/delete 才维护 digest-derived fallback tag。subject push 与 fallback index 使用 repository + subject digest lease；单实例 backend 在 lease/fencing 保护下重新读取、执行标准 manifest GET/PUT/DELETE 的 read-modify-write，并在写后 GET 验证 tag digest，不假设 Distribution 提供通用 `If-Match` 条件写入；冲突最多重放 5 次。同步失败返回 503 和 Retry-After，已写入的 manifest 保留，任务进入 repair_pending。
- fallback 模式下删除先更新 fallback index，再删除 referrer manifest；部分完成进入 repair_pending，修复任务可重放且幂等。native 模式下直接使用 Distribution 的标准 manifest DELETE。
- fallback tag 名称严格遵守 OCI Referrers Tag Schema：对 subject digest `algorithm:encoded`，algorithm 截断到 32 字符、encoded 截断到 64 字符，使用 `algorithm-encoded` 拼接，并把不符合 tag 字符集的字符替换为 `-`。该 tag 由 backend 保留，但允许外部 GET/HEAD 和 tags/list 读取；外部通过普通 tag PUT、digest PUT 的 `tag` 参数、tag DELETE 或当前 fallback index manifest 的 digest DELETE 覆盖/删除返回 409，其他 tag 不受影响。backend 自己完成 fallback index 更新后也必须在成功的 subject manifest PUT 响应中返回 `OCI-Subject: <subject digest>`；只有 manifest 已写入但 index 未完成时，才返回 503 且不宣称 subject 已处理。
- fallback index 损坏时不自动覆盖；受保护的 reconcile 才能修复。replace/prune 必须具备独立权限、complete=true，构建并完整验证新 index 后再在 backend lease/fencing 保护下写入。

fallback tag 存在时（包括合法但为空的 index），它是本地 Referrers 结果的权威来源，不再查询公网。公网 Referrers 查询必须遵守 04-upstream-mirror.md 的认证、代理、TLS、重定向、SSRF 和重试规则。

如果 Distribution 从 fallback tag 模式升级或切换到 native Referrers API，必须先保证 native 结果包含既有 fallback index 中的 subject manifest；这是 OCI 对启用 Referrers API 的升级要求。不能仅因为探测到 native 2xx 就直接丢弃尚未迁移的 fallback 数据，升级/迁移由受保护 reconcile 完成并记录 generation。

fallback descriptor 的 digest、size、mediaType 来自实际 referrer 内容校验；manifest 的 `artifactType` 存在时沿用，缺失时使用其 config descriptor 的 mediaType，index 的空 artifactType 保持省略；合法 annotations 从 referrer manifest/index 派生，不生成 descriptor.data；artifactType 过滤必须精确匹配。

fallback 模式的 repair intent 以 repository、subject digest 和 referrer digest 为键，记录 add/remove、generation 和期望状态。repair worker 只消费已持久化且到期的 intent，不扫描 catalog、tags、Distribution storage 或临时缓存。新 generation 覆盖旧 intent，修复完成后通过条件写入清除当前 generation。add 需要重新通过标准 Registry API 校验 referrer manifest，remove 对目标不存在按幂等成功处理。

暂时性 Distribution/storage 故障、lease 冲突和写后验证冲突继续退避重试；subject、digest、size、mediaType 校验失败以及 fallback index 损坏保留 repair_pending，设置 retryable=false，不再自动调度，只能由受保护的 retry/reconcile 处理。

受保护 reconcile 的默认候选只能来自完整跟随分页并确认没有下一页的原生 Referrers API，或来自管理请求明确提供的列表；管理请求提供的列表默认不表示完整集合。replace/prune 必须携带 complete=true 和独立的 referrer-prune 权限，只有完整校验的新 index 才能条件替换现有 index。

## 9. Backend 与 Distribution 的通信原则

除 Distribution 官方 GC 执行器外，backend 对内容的所有读取、写入、删除、tag 管理、referrer fallback 维护和固化导入都使用标准 Registry HTTP API。backend 不读取 Distribution 的 storage layout，不依赖私有 link 文件，不通过数据库或文件直接改内容。内部 HTTPS、认证和错误映射见 06-auth-security-and-limits.md。
