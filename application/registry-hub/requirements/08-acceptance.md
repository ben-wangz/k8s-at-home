# 08. 验收清单

本清单替代旧文档中重复的 1-92 编号验收条目。每条验收都应在实现、故障注入和重启恢复测试中有可观测结果。

## 1. 路由与 Registry API

- [ ] 普通 manifest/blob GET/HEAD 先查询 I(R) 对应的内置 repository/reference；只有内置 404 才能回源，tags/list 和 Referrers 遵守各自回退规则。
- [ ] 内置 401、403、405、429、5xx 和 storage 错误不会被当作 miss。
- [ ] 内置 Distribution 的 401/403 不会泄露内部 challenge、Service 或凭据；401 只允许一次匹配内部配置的 challenge 重试，仍失败时按 backend 依赖故障返回标准 502/503 错误。
- [ ] busybox 这类不含 / 的仓库名只查本地，不能访问公网。
- [ ] docker.io/library/busybox、library/busybox 和 host:5000/team/image 按第一段字面解析。
- [ ] host:port、方括号 IPv6 authority、非法端口、userinfo、未加方括号 IPv6 和控制字符按 authority 解析规则处理；非法 authority 不建立网络连接。
- [ ] 不依赖 upstream 列表、upstream ID、mirror/local 前缀或 Docker Hub short-name 展开。
- [ ] 逻辑 repository R 原样保留；发往内置 Distribution 时，`docker.io`、`host:5000`、IPv6 和 `localhost` 等 domain-like 首段使用可逆 I(R) 映射，`library` 等普通首段可保持原样。
- [ ] URL path 只解码一次，编码 slash、反斜杠、路径穿越、空段、重复编码和非法 endpoint 被拒绝。
- [ ] endpoint 右侧匹配正确，不因仓库名包含 manifests、blobs、tags 或 referrers 而误路由。
- [ ] POST、PATCH、PUT、DELETE、upload session、`_catalog` 和 tag 操作不会发往公网；upload 状态查询/取消的本地 404 也不回源。
- [ ] local push 和 mirror 固化可以使用相同 repository；同 tag 不同 digest 后完成者收到 409，同 digest 重复发布幂等。
- [ ] manifest digest PUT 支持 OCI 的重复 `tag` query 参数；backend 可通过多个标准 manifest PUT 完成，保护 tag/竞争规则逐个生效，并通过 `OCI-Tag` 只报告实际接受的 tag。
- [ ] manifest/index、blob、upload、tags/list、referrers 的标准状态码和 header 能被 Docker/OCI 客户端理解。
- [ ] manifest/index PUT 校验 body `mediaType` 与 `Content-Type`，digest reference 使用 Distribution 对该 media type 的 digest 语义，成功返回 `201`、可继续访问的 `Location` 和正确的 `Docker-Content-Digest`，原始 bytes 不被重写。
- [ ] `/v2/` 返回标准 200/401、`Docker-Distribution-API-Version` 和 challenge；backend 错误使用标准 `errors[]` JSON envelope。
- [ ] upload 使用标准 trailing-slash endpoint、single-request `POST ?digest=`、非 sha256 分块的 `digest-algorithm`、重复 digest 完成参数、202/204/416/201 状态、Range、Content-Range、chunked transfer、offset、mount fallback 和 opaque Location；Location 始终可由客户端继续访问 backend，不能暴露 storage URL。
- [ ] Distribution storage redirect 默认关闭；显式开启时必须配置 storage endpoint allowlist，backend 只在内部跟随并代理，逐跳执行 TLS、scheme、DNS、IP、端口、SSRF 和 redirect loop 检查。
- [ ] Distribution storage redirect 不向 storage 转发客户端 Authorization、Cookie 或 proxy credential，也不向客户端泄露 storage Location、内部 hostname 或签名 URL；目标校验失败、签名过期或 redirect 不安全时不会自动改走其他地址。
- [ ] tags/list 的本地 200 不与公网合并，响应中的 `name`/`Link` 不泄露 I(R)；local 404 且仓库含 / 时可以查询公网分页结果，不创建固化任务；无 / 时不回源；_catalog 默认关闭，启用时反向映射 I(R)，且不回源。
- [ ] tags/list 接受 `n=0` 并返回空列表且不带下一页 Link；`last` 可以不带 `n`，未指定 `n` 时不发生无 Link 的静默截断，其他分页参数遵守 tag 合法性、顺序和上限。
- [ ] Referrers 成功返回 `200`、OCI image index media type 和空/非空 `manifests` 数组；descriptor 的 artifactType、annotations、分页 Link 和 `OCI-Filters-Applied` 符合 OCI 规则，backend 扩展的 `n/last` 不泄露 native Distribution 或上游 cursor。

## 2. Mirror 与内容验证

- [ ] manifest/index 使用支持范围内完整的 Docker Schema 2、manifest list 和 OCI Accept discovery。
- [ ] 客户端接受 discovery index 时交付原始对象；不接受时最多执行一次兼容请求。
- [ ] 兼容请求失败不阻止已验证 discovery 根的后台固化。
- [ ] 无可接受 media type 返回 406，不创建错误缓存或错误固化任务。
- [ ] digest 请求不会被其他 media type、index 或平台对象替换。
- [ ] HEAD 不执行 GET fallback，不写缓存、不创建任务、不产生 body；上游 HEAD 405 不自动 GET。
- [ ] 公网完整 body 的请求/descriptor digest、声明 size 和结构校验通过后才能消费；只有客户端请求 Range 时才接受 206，206 只校验片段范围和长度，完整 digest 在合并全部对象后验证；HEAD 不产生 complete，缺少关键 Registry 元数据时不自动 GET；`Docker-Content-Digest` 按 media type 做附加校验，不能替代本地/request digest。
- [ ] descriptor.urls 原样保留但永不访问；标准 blob API 缺失时根对象不发布；Distribution 配置不会因合法 URL descriptor 或非本地平台而拒绝可保存 artifact。
- [ ] descriptor.data 正确 Base64 解码并验证 size/digest；非法 data 不产生 complete 或任务。
- [ ] manifest/index 原始 bytes、media type、annotation、platform、subject 和 descriptor 扩展字段不被改写。
- [ ] Docker image、OCI image/index、Helm OCI、ORAS binary、SBOM、签名、provenance、Wasm 和未知合法 artifact 都能透明处理。
- [ ] index 的所有平台和递归子 index/manifest 都被固化，不按客户端 platform 裁剪。
- [ ] 普通 manifest 只递归 config/layer 作为对象依赖，不自动递归 subject/referrer。
- [ ] 合法的带 subject manifest 在 subject 尚未存在时也可以发布，不能把 subject 缺失误报为普通 blob 缺失。
- [ ] shared digest 可复用，但每条引用边仍校验 size/digest/metadata。
- [ ] active DFS 循环、size 冲突、递归类型冲突和资源限制错误为永久失败，不重试、不发布根。

## 3. 异步固化和并发

- [ ] 会触发 artifact 固化的公网 manifest/index GET 先持久化任务状态并预留队列配额，失败时不访问公网并返回 503/Retry-After；HEAD、tags/list、Referrers discovery 和仅 blob GET 不单独创建固化任务。
- [ ] 内置 Registry 命中不受固化队列限制。
- [ ] 任务重启后由持久化 state/graph/checkpoint 恢复，内存队列丢失不影响正确性。
- [ ] worker 按拓扑顺序通过标准 Registry API 上传 blob、manifest/index 和根 reference，并进行读取验证。
- [ ] 成功条件是完整闭包和根 reference 均可读；成功后缓存按宽限期清理。
- [ ] 对象下载 lease key 包含 authority、逻辑 repository R 和 digest。
- [ ] 对象 lease 默认 TTL 60 秒、20 秒续租，旧 leader fencing 失败，接管最多一个新 leader。
- [ ] waiter 不重复公网下载，最多等待 30 秒后返回 503/Retry-After。
- [ ] Range checkpoint 绑定对象身份、声明 size、offset 和 fencing token。
- [ ] solidification worker 全局最多 4 个，repair_pending 优先但保留 1 个新固化槽位。
- [ ] retryCount、nextAttemptAt、checkpoint 和错误终态持久化；永久错误不自动重试。
- [ ] 取消在根、tag、fallback index 发布前生效；已上传对象不回滚，根已发布时取消返回 409。
- [ ] 删除推进 generation/tombstone 后，旧任务不能再发布；generation counter 不被清理。

## 4. Storage

- [ ] FileSystem 和 S3 使用相同的 storage 接口和错误分类。
- [ ] data、metadata、complete 使用两阶段提交；未验证对象不可消费。
- [ ] FileSystem 使用临时文件、fsync、原子 rename 和目录 fsync。
- [ ] S3 支持 multipart、条件写入/删除、TLS 默认校验和不确定提交后的读取确认。
- [ ] S3 使用 HTTP 必须有独立显式开关；自定义 CA 可用，跳过校验不可作为默认。
- [ ] task state、graph、lease、repair intent 和 generation 使用 schemaVersion 与条件更新。
- [ ] 当前及上一兼容 schema 可读，更高/不兼容 schema 被隔离，不被覆盖、删除或消费。
- [ ] cleanup_pending 是缓存对象状态而不是固化任务终态；repair_pending 及 repair intent 不会被普通失败保留策略清理。
- [ ] List 仅用于恢复、孤儿扫描和清理，不作为提交或引用存在性的依据。
- [ ] storage 故障产生 warning，并阻止回源、未确认对象消费、根发布、tag 更新和删除。
- [ ] 清理顺序为缓存对象、引用图、任务状态；状态缺失/损坏/不确定时保留对象。
- [ ] 共享对象清理使用条件版本和引用图检查，不依赖简单引用计数。
- [ ] staging、multipart、条件操作 unknown commit、lease 丢失和进程重启都可恢复。

## 5. 上游认证、代理和安全

- [ ] 客户端 Authorization 永不发送到公网。
- [ ] 未配置 authority 凭据时只能访问 anonymous upstream。
- [ ] 每个公网 authority 的 Basic/Bearer 凭据独立配置、内存使用且 scope 为 pull。
- [ ] challenge 最多一次重试；公网只读请求允许跨 host redirect，但每一跳都经过 TLS、端口、DNS、SSRF 和 scheme 检查，且不携带源 host 凭据，目标 host 只能使用自身配置的认证；
- [ ] backend 默认不跳过上游 TLS；HTTP 需要独立显式配置。
- [ ] 单独 upstream proxy 优先于全局应用 proxy；HTTP_PROXY、HTTPS_PROXY、NO_PROXY 按进程全局规则覆盖最终选择；代理一旦选定，失败、超时或重试耗尽不会自动直连，只有未配置代理或 NO_PROXY 命中时才直连。
- [ ] proxy credential 与客户端、内部 Distribution、上游 Registry credential 隔离。
- [ ] DNS 多地址、rebinding、loopback、私有/保留地址、metadata、非允许端口和 redirect 均被 SSRF 防护。
- [ ] HTTPS 降级、非 HTTP scheme、redirect loop 和超跳数被拒绝。
- [ ] 跨 host redirect 的目标 URL 不暴露给客户端；redirect 安全失败时不写入完整缓存、不创建或推进固化根。
- [ ] 前台 GET/HEAD/token 最多 3 次，只重试网络错误、408、429、502、503、504；其他 5xx 默认不自动重试，不缓存错误响应。
- [ ] Retry-After、总 deadline、后台 retryCount 和 checkpoint 恢复符合 04 模块要求。

## 6. Referrers

- [ ] 原生 referrers 2xx（包括空结果）是本地权威结果，不与 fallback 合并。
- [ ] 只有原生 404 才读取 digest-derived fallback tag；其他错误/格式非法不回退。
- [ ] native capability 只在进程内存缓存；原生 404 最多缓存 30 秒，其他错误/格式非法立即使能力缓存失效。
- [ ] fallback tag 缺失且仓库含 / 时可以查询公网 Referrers；fallback tag 存在（包括空 index）时不查询公网，公网结果不自动下载或固化。
- [ ] Referrers 支持 artifactType、OCI-Filters-Applied、分页 Link 和绑定 subject/filter/index digest 的 cursor；index 变化或 cursor 无效返回 409。
- [ ] fallback index 通过标准 manifest API 维护，不读取 Distribution 私有存储。
- [ ] fallback index 的并发更新依赖单实例 backend 的 repository/subject lease、fencing、read-modify-write 和写后 GET 验证，不假设 Distribution 提供通用条件写入接口。
- [ ] native Referrers API 返回 2xx 时，subject push/delete 使用标准 Distribution manifest API，push 成功返回 `OCI-Subject: subject digest`，不额外维护 fallback index；fallback 模式在 index 成功更新后同样返回该 header，避免客户端再次自行更新保留 tag。
- [ ] native Referrers API 只有返回 404 时才维护 fallback index；subject push 使用 subject lease、repair intent、lease/fencing 保护下的 read-modify-write 和写后 GET 验证，索引同步失败返回 503/Retry-After，已写入 manifest 保留并进入 repair_pending。
- [ ] fallback tag 遵守 OCI 的截断、`-` 拼接和非法字符替换规则；受保护 tag 可读但不可被外部覆盖/删除。
- [ ] 部分成功进入 repair_pending，修复可重放；损坏 index 不自动覆盖。
- [ ] artifactType 过滤先于分页，Link/cursor 绑定 subject、filter、index digest 和最后 digest。
- [ ] fallback 保留 tag 可被 GET/HEAD/tags/list 读取，外部覆盖/删除返回 409。
- [ ] Distribution native Referrers 启用或升级前，既有 fallback tag 中的 subject manifest 已被纳入 native 结果；不能因 native 2xx 空结果丢失未迁移的 referrer。

## 7. 删除和 GC

- [ ] 删除不回源；manifest digest、tag reference、blob 和 referrer 的删除语义符合 05 模块及 Distribution/OCI 标准状态码。
- [ ] Distribution 删除启用时普通 tag reference 删除返回 202，目标不存在返回 404；删除被禁用时保留 400/405；受保护的 fallback referrers tag 通过普通 tag PUT、digest PUT 的 `tag` 参数、tag DELETE 或其当前 index manifest 的 digest DELETE 操作返回 409；manifest digest 删除不额外因其他普通 tag 拒绝；blob DELETE 遵守标准 202/404 语义；删除不级联删除 blob 或无关 referrer。
- [ ] GC 默认关闭；管理面触发需要独立 gc-admin 权限。
- [ ] draining 拒绝新写入，默认最多等待 5 分钟；只有确认 backend 已排空已接纳写入、没有新的写入 admission、只有 backend 能访问 Distribution 且没有其他 storage writer 后才进入 `backend_quiesced`/`gc_running`；Distribution 继续运行时允许读取。
- [ ] GC failed/unknown 保持写入封禁，不支持重启、删锁或无条件强制解锁。
- [ ] 单机在完成 backend 闸门、drain、唯一写入口和后台 writer 检查后，通过绝对路径执行官方 Distribution GC executable；只读/停止生命周期适配是可选纵深防御，启用时必须先确认其状态。
- [ ] Kubernetes 使用同版本/配置/storage 的独立 Job，不使用 kubectl exec 或 Service；Job 启动前已完成 backend 闸门和唯一写入口检查，若配置了只读/停止防御则其状态也已确认。
- [ ] GC 前 Distribution 的 upload purging 等后台 writer 已关闭；NetworkPolicy/Service 证明只有 backend 可以访问 Distribution；无法证明 storage 无并发写入时，GC 不会启动，或必须先启用只读/停止防御。
- [ ] Job 的 parallelism/completions 为 1、backoffLimit 为 0、restartPolicy 为 Never。
- [ ] Job identity、UID、配置摘要和最终状态持久化；watch 断开不会被误判为成功。
- [ ] GC 成功后，已启用的生命周期防御先恢复，Distribution 可读验证和状态条件写入完成后才恢复 open；未启用时也必须重新验证 Distribution、storage 和唯一写入口。

## 8. 运维和认证

- [ ] backend/Distribution 分离部署，Distribution 只允许 backend Service 访问。
- [ ] 未配置 client auth 时按 no-auth 启动并写 warning；已配置但错误的认证配置不会静默降级；no-auth 允许可信网络中的 Registry 操作但仍受闸门和资源限制；启用认证时权限默认 deny；客户端和管理面凭据独立。
- [ ] backend 使用内部 Distribution 自签 CA 默认校验，不实现额外客户端 CA/mTLS。
- [ ] 所有秘密不进入 Helm values、日志、任务状态、cache metadata 或 metrics label。
- [ ] 单实例、backend/Distribution 持久存储配置（FileSystem 模式需要的 PVC 或对象存储配置）、探针、启动顺序、升级和故障重启由 Helm/Kubernetes 负责。
- [ ] 管理接口支持任务查询、retry/cancel/cleanup、referrer reconcile 和 GC status/trigger/retry/recover。
- [ ] 日志为结构化 JSON，storage 故障、认证、SSRF、digest mismatch、fencing 和 GC unknown 可检索。
- [ ] 指标保持低基数，不把 repository、tag、digest、token 或完整 URL 作为无限 label。
- [ ] 优雅停止保留 checkpoint、任务状态、complete 和 tombstone；重启可以恢复。
