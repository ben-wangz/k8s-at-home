# 07. 部署、运维与可观测性

## 1. Helm 交付拓扑

registry-hub 通过自己的 Helm chart 交付 backend 和 Distribution：

- backend 与 Distribution 使用分离的 Deployment/Pod 和 Service，不使用 sidecar；
- backend Service/Ingress 是唯一外部入口；
- Distribution Service 为内部 HTTPS Service，不配置外部 Ingress、NodePort 或 LoadBalancer；
- Distribution 使用官方最新稳定版本镜像，部署时应固定明确的稳定版本或不可变 digest，不使用漂移的 `latest` 标签；chart 只依赖该镜像，不引入 Distribution Helm chart；
- Distribution 必须启用标准内容删除能力，以支持 manifest、blob 和允许的 tag 删除；选择的稳定版本必须包含当前删除路径的安全修复，并验证 `storage.delete.enabled=false` 时 tag/digest 删除都不会绕过配置；backend 仍通过认证、generation 和 GC 闸门控制实际操作；
- Distribution 的 `maintenance.uploadpurging` 等后台写入任务必须关闭；GC 前 backend 必须证明自己是唯一写入口、已排空所有已接纳写入且不存在其他 storage writer。Distribution 以 `maintenance.readonly.enabled=true` 配置重启/替换或停止进程是无法证明该条件时的可选纵深防御，不是每次 GC 的强制前置；
- Distribution 的 storage redirect 默认关闭，使 blob 内容经过内部 Service 返回给 backend；如果部署明确开启 redirect，必须同时配置允许的 storage endpoint，backend 只能在内部跟随并安全代理这些目标。每一跳都重新执行 HTTPS、scheme、DNS、IP、端口、SSRF 和 redirect loop 检查，不向 storage 发送客户端 Authorization、Cookie 或 proxy credential，也不能把 Location、内部 hostname 或签名 URL 暴露给客户端；目标校验失败、签名过期或 redirect 不安全时直接返回错误，不自动改走其他地址；
- Distribution 的 manifest URL 校验和 index platform 校验不能配置成拒绝本项目要透明保存的内容；chart 必须显式配置 `validation.manifests.urls.allow`（不能留空导致含 URL 的 manifest 默认被拒绝）。`validation.manifests.indexes.platforms` 是“引用的平台 manifest 是否必须已存在”的校验，不是平台 allowlist；使用 `all` 保持完整 index 校验，backend 在固化时先上传全部平台对象再上传 index，不使用 `list` 造成只校验部分平台，也不依赖 `none` 绕过闭包完整性。`descriptor.urls` 只允许被存储和返回，backend 永不访问；多架构 index 的所有平台都必须固化，资源限制由 backend 自己执行；
- backend 与 Distribution 均按单实例部署；
- Distribution 使用一个单写入的持久存储配置，可以是 FileSystem/PVC 或 Distribution 支持的对象存储；backend 不依赖其存储实现和布局；
- storage 抽象按配置选择 FileSystem 或 S3，临时缓存和任务状态必须持久化；
- NetworkPolicy 只允许 backend 访问 Distribution；
- Secret、内部自签 CA、S3 凭据和上游凭据通过 Secret 或 workload identity 注入。

chart 需要提供 backend、Distribution、Service、Ingress、NetworkPolicy、Secret 引用、按 FileSystem 模式需要的 PVC 或对象存储配置、探针和 GC 执行器所需的模板。具体集群的 Ingress controller、StorageClass、网络插件和调度兼容性不是 registry-hub 协议需求。

## 2. 启动、健康和故障恢复

Helm/Kubernetes 负责启动顺序、探针、故障重启和升级顺序；backend 仍需对依赖状态做业务级 fail-closed：

- backend 启动后检查配置、storage、内部 CA/TLS、Distribution /v2/ 和必要的认证；
- 未配置客户端 auth 时按 no-auth 允许启动并写 warning；已配置但格式错误或 Secret 无法读取时启动或 readiness 失败，不能静默降级；
- storage 不可用、内部证书校验失败或 Distribution 返回非预期状态时，不宣称可接收需要依赖它的业务请求；
- liveness 只反映进程是否能运行，不因公网 Registry 暂时不可用而重启；
- readiness 反映配置、storage 和内部 Distribution 的可用性；
- 公网上游不可用不会使 backend 进程退出；
- storage 故障使用 warning 日志，并阻断回源、对象消费、根发布、tag/fallback index 更新和删除；
- GC 状态 unknown/failed 时保持写入闸门封禁，不靠进程重启解锁；
- 重启后从持久化 task state、graph、lease、generation 和执行器 identity 恢复，不依赖内存队列。

## 3. 配置和版本边界

backend 是 Go 二进制，配置从环境变量读取。建议配置项和 Secret 引用见 06-auth-security-and-limits.md。配置变更需要明确的 reload 或滚动重启语义；不允许只更新 Helm values 而让已有进程使用未知的半套配置。

版本升级必须：

- 保持 Distribution 镜像、GC executable/Job 和 registry storage 配置一致；
- 使用 storage schemaVersion 的当前/上一兼容版本读取策略；
- 不在升级时做启动全量 cache/task 扫描；
- 升级期间保留任务、checkpoint、lease、generation 和 repair intent；
- backend 不能在未知版本对象上执行覆盖、删除或固化；
- Distribution 内容操作仍只通过标准 Registry API；
- 不要求旧域名、旧端口、双写、回滚兼容或迁移旧服务的私有数据布局。

集群如何实现发布、滚动升级、PVC 或对象存储配置迁移和具体停机窗口由 Helm/Kubernetes 决定，不在本项目中维护第二套部署编排。

## 4. 管理接口

管理面使用与客户端隔离的内部 listener 和认证。至少提供：

- GET /api/v1/tasks：按状态、authority、repository、digest、时间分页查询；
- POST /api/v1/tasks/<id>/retry：重试 retryable 任务；
- POST /api/v1/tasks/<id>/cancel：取消尚未发布根的任务；
- POST /api/v1/tasks/cleanup：触发受保护清理；
- GET/POST /api/v1/referrers/reconcile：按 repository + subject 修复 fallback index；
- GET /api/v1/gc：查询闸门和执行器状态；
- POST /api/v1/gc：触发 GC；
- POST /api/v1/gc/retry：确认无其他执行器后重试；
- POST /api/v1/gc/recover：通过执行器 identity/Job 状态 reconcile 后恢复闸门。

管理接口必须分页、有权限、低基数审计，并返回任务 ID、状态、retryAfter、错误分类和 generation；不能返回凭据、完整上游 URL、内部 storage key 或敏感 manifest 内容。

## 5. 日志

使用结构化 JSON 日志，字段包括 request ID、trace ID（如有）、operation、authority、repository、reference/digest、status、latency、cache source、task ID、retry count、storage backend、GC gate state 和 error class。

必须：

- storage 故障写 warning；
- no-auth 配置启动写 warning；
- digest/size mismatch、SSRF、TLS、认证、GC unknown 和 fencing 失败使用稳定错误分类；
- 不把每个 repository、tag、完整 URL 或 token 作为高基数指标 label；
- 不记录 Basic/Bearer、proxy credential、Secret、带 query 的 token URL 或完整客户端 Authorization；
- 不在错误 body 中回显上游私密响应。

## 6. Metrics

至少提供以下低基数指标：

- 请求按 operation、method、status、cache source、错误分类的计数和延迟；
- local hit、upstream fetch、upstream 404、认证 challenge、重定向、retry 和 digest mismatch；
- active/waiting object lease、lease lost、download waiter timeout；
- task 按状态、authority 类别、错误分类的数量；
- cache bytes/objects、staging/orphan、cleanup pending；
- storage 操作按 operation/result 的计数、延迟和 warning；
- Distribution API 错误和响应验证失败；
- referrer native/fallback、repair_pending、reconcile 结果；
- GC gate state、执行时长、成功/失败/unknown；
- 当前各类并发闸门和 rate limiter 拒绝。

authority、repository、tag、digest、客户端身份和 token scope 不应直接作为无限基数 label。需要关联具体对象时使用 request/task ID，详细内容放在受控日志或管理查询中。

## 7. 关闭与优雅停止

backend 停止时：

1. readiness 先变为 false；
2. 停止接受新公网回源和新固化入队；
3. 允许短窗口内的本地 Registry 请求完成；
4. 续租并持久化 worker 状态，不能在退出前删除可恢复 checkpoint；
5. 释放或让 lease 按 TTL 过期；
6. 不删除 staging、complete、task state 或 tombstone；
7. 进程重启后按恢复规则继续。

Distribution 的普通停止、存储挂载/连接和 Job 生命周期由 Helm/Kubernetes 负责；如果部署选择 GC 的只读/停止纵深防御，该操作必须通过明确配置的生命周期适配完成。backend 不能把 Distribution read-only 当作运行时 API，也不能因自身优雅停止或 GC Job 退出而推断 GC 已完成。

## 8. 部署边界总结

项目只关心 backend、标准 Distribution、抽象 storage、认证/网络安全和标准 Registry 行为。具体集群部署如何兼容、Ingress controller 如何配置、StorageClass 如何提供卷、Pod 如何调度以及高可用如何实现，不属于本项目需要额外抽象的功能。
