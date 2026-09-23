# 03. Storage 抽象、临时缓存与任务状态

## 1. 适用范围

backend 不把公网临时缓存、任务状态、引用图或 lease 绑定到某一种后端。必须抽象 storage 层，并提供两种实现：

- FileSystem：适合单机部署和挂载持久卷；
- S3：适合对象存储部署。

实现优先采用成熟、活跃且支持所需条件写入、条件删除、multipart 和 lease 语义的 Go 开源库；业务代码通过本抽象层使用它们，不能把具体库的 API 或错误类型泄露到上层。如果库不能提供等价的原子/条件语义，必须由适配层补齐或在启动时拒绝该后端。

内置 Distribution 的真实内容存储不由该抽象层接管。backend 只能通过标准 Registry HTTP API 访问 Distribution。storage 抽象只负责临时缓存、任务状态、引用图、lease、repair intent、generation/tombstone 和清理元数据。

backend storage 与 Distribution registry storage 即使物理上共用同一个 FileSystem 或 S3 服务，也必须使用独立的配置和逻辑命名空间；backend 不把 Registry 内容对象当作临时对象，也不通过该抽象层列举、修改或删除 Registry storage。

## 2. 逻辑命名空间与 key

逻辑命名空间至少包括：

- cache：公网对象的 data、metadata 和 complete；
- tasks：固化/修复/清理任务的 state、引用 graph 和 checkpoint；
- leases：任务 worker、对象下载、tag、subject referrer、GC 和清理协调 lease；
- generations：repository/reference 的 generation 和 tombstone。

典型 key：

    cache/objects/<algorithm>/<digest>/data
    cache/objects/<algorithm>/<digest>/meta
    cache/objects/<algorithm>/<digest>/representations/<encoded-media-type>
    cache/objects/<algorithm>/<digest>/complete
    tasks/<task-id>/state
    tasks/<task-id>/graph
    leases/objects/<stable-hash>
    leases/tasks/<task-id>
    leases/tags/<stable-hash>
    leases/referrers/<stable-hash>
    generations/<repository>/<reference>

key 必须使用不透明、可验证的编码。不能把未校验的用户 path 直接拼接成本地文件路径，也不能让 S3 key 逃逸配置 prefix。

## 3. Storage 接口契约

抽象接口至少提供：

- Get、Head；
- 流式 Put；
- PutIfAbsent；
- PutIfMatch；
- Delete；
- DeleteIfMatch；
- 带 continuation token 的 List；
- Acquire、Renew、Release lease；
- 版本/ETag 条件 token；
- 关闭或取消时的 context 传播。

接口返回稳定的抽象错误：

| 错误 | 语义 |
| --- | --- |
| NotFound | 对象不存在，属于正常分支 |
| AlreadyExists | PutIfAbsent 已有对象 |
| ConditionFailed | 版本、ETag 或 fencing 条件不匹配 |
| LeaseLost | lease 已过期、被接管或 fencing 失败 |
| Unavailable | 后端暂时不可用 |
| Timeout | 操作超时，提交状态可能未知 |
| PermissionDenied | 凭据或权限错误 |
| InvalidKey | key 不符合抽象层约束 |
| CorruptObject | metadata/data/complete 校验失败 |
| UnknownCommit | 提交或删除结果不确定，必须先读取确认 |

业务代码不得依赖 S3 的具体 ETag 算法、FileSystem inode、目录存在性或 List 顺序来实现正确性。不存在跨对象事务，恢复必须依靠幂等操作、条件写入和重新校验。

## 4. 临时对象提交协议

所有可消费临时对象都使用两阶段提交：

1. 将 data 流式写入 staging key 或临时文件；
2. 完整读取并校验实际 digest、声明 size、资源限制和必要的上游 header；
3. 写入 metadata，其中包含 schemaVersion、algorithm、digest、size、已验证 mediaType 集合或 representation 记录、来源、创建时间和对象版本；
4. 条件创建不可覆盖的 complete 标记；
5. 只有同时读取并验证 data、metadata、complete 后，对象才可供响应、固化或其他任务消费。

complete 只能条件创建，不允许覆盖。data 或 metadata 写入成功但 complete 缺失的对象是 staging/orphan，只能由恢复和清理流程在确认没有引用后处理。digest mismatch、size mismatch、结构校验失败和资源限制错误不能产生 complete。

同一 digest 的 data 只能保存一份，但 manifest/index 的不同已验证 media type、Accept representation 和来源元数据不能互相覆盖；可以使用 representation 子 key，也可以在版本化 metadata 中保存集合。任务根身份仍保留 digest + mediaType，响应必须从对应 representation 读取，不能把一个 media type 静默改写成另一个。

task state、graph、lease、repair intent 和 generation 也必须使用 schemaVersion、版本条件和 fencing token，禁止无条件覆盖其他 worker 的状态。

## 5. FileSystem 实现

FileSystem 实现必须：

- 使用配置的绝对 root path；
- 拒绝 path traversal、绝对 key 和非法 key；
- 通过临时文件写入、fsync、原子 rename 和目录 fsync 提供提交边界；
- 不把目录存在性当作对象提交证明；
- 在进程异常退出后识别 staging、orphan 和不完整 complete；
- 对条件写入使用版本文件、锁或等价的原子机制，并返回抽象 ConditionFailed；
- 对 lease 保存过期时间、owner 和 fencing token，不能依赖进程内 mutex。

单机部署时，backend 和 Distribution 是同一 OS 上的两个不同进程；backend 可以通过绝对路径调用 Distribution GC 执行器，但不能因此读取或修改 Distribution storage。

## 6. S3 实现

S3 实现至少支持 endpoint、bucket、prefix、region、凭据或 workload identity、multipart threshold、part size、超时和并发配置。凭据从 Secret 或 workload identity 注入，不写 Helm values、日志或任务状态。

- 默认校验 TLS 证书，可配置自定义 CA；
- 使用 HTTP 必须由独立的显式 allow-insecure 配置开启，不能只把 endpoint 改成 http；
- multipart 上传必须在 complete 前完成实际内容验证；
- multipart complete、条件写入、条件删除结果不确定时，先读取 metadata 和对象内容确认，禁止盲目重复提交或删除；
- 使用 S3 条件语义实现 PutIfAbsent、PutIfMatch、DeleteIfMatch；若目标后端不提供等价能力，启动时 Ready 失败；
- 权限应按最小范围限制到配置 prefix 下所需的读、写、列举和删除操作；
- S3 访问失败、权限失败或一致性探测失败时，backend warning 日志并 fail-closed。

HTTP_PROXY、HTTPS_PROXY 和 NO_PROXY 由 Go 进程按 Linux 程序规则作为全局环境生效；storage 的具体 endpoint 不改变这一规则。公网 Registry 的单独 proxy 规则见 04-upstream-mirror.md。

## 7. Lease 与 fencing

lease 由 storage 持久化，不是进程内锁。每个 lease 包含 key、owner、fencing token、获取时间、过期时间、续租时间和状态。

默认租约：

| 用途 | TTL | 续租 |
| --- | ---: | ---: |
| 固化任务 worker | 60 秒 | 20 秒 |
| 对象下载 leader | 60 秒 | 20 秒 |
| tag/reference 竞争 | 30 秒 | 10 秒 |
| subject referrer/fallback index | 30 秒 | 10 秒 |
| GC/写入闸门 | 由 GC 执行状态决定 | 持续心跳 |

fencing token 必须在每次 checkpoint、状态更新和发布前验证。lease 丢失的 worker 不得继续写 complete、任务根、tag、fallback index 或清理对象。对象下载 leader 接管、waiter 超时和 solidification worker 的行为由 02-artifacts-and-solidification.md 定义。

## 8. Schema 版本与迁移

meta、complete、task state、graph、lease、repair intent 和 generation 都带 schemaVersion：

- backend 支持当前版本和上一个兼容版本读取，只写当前版本；
- 旧版本按需迁移，使用条件写入；迁移中断保留旧对象并可重试；
- immutable blob data 不迁移；
- 更高版本或不兼容版本的对象不得覆盖、删除或消费，相关任务隔离并暂停；
- 单对象访问、任务恢复和受保护管理操作可以触发迁移；
- 不执行启动时全量扫描，不以“扫描完成”作为 readiness 条件；
- 迁移和恢复可中断、可恢复，不能依赖跨对象事务。

## 9. 清理与保留

默认保留策略：

- 成功 complete cache：完成后至少保留 1 小时宽限期；没有关联固化任务的直接 blob GET 产生的 complete 对象也遵守独立的直接缓存 TTL，TTL 到期且没有新引用或 active lease 时进入 cleanup_pending；
- failed（包括 conflict 错误分类）、cancelled 任务或关联 cache：至少保留 7 天；
- repair_pending 任务和其 repair intent 不受普通失败保留期清理，直到修复完成或受保护 reconcile 明确关闭；
- task state：至少保留 30 天；
- cleanup scan：默认每 5 分钟；
- orphan/staging scan：默认每小时。

缓存对象至少区分 staging、complete、cleanup_pending 和 isolated 生命周期。solidified 任务关联的 complete 对象在宽限期后进入 cleanup_pending；没有固化任务的直接缓存按直接缓存 TTL 清理；校验损坏、schema 不兼容或来源不确定的对象进入 isolated，不得消费或直接删除。

清理顺序为 data/metadata/complete 缓存对象、引用图、任务状态；引用图在关联缓存清理或隔离完成前必须保留，任务状态最后删除。任何任务状态、graph、lease 或版本读取缺失、损坏、不兼容或结果不确定时，不能把缓存视为无引用，也不能依靠 List 直接删除。

共享对象不使用简单引用计数。删除前重新读取并确认 complete、metadata、data、引用图、active lease、对象版本和 deletion generation；出现新引用、lease、generation 变化或条件失败则放弃本次清理。DeleteIfMatch 结果 UnknownCommit 时先读取确认。

storage 故障必须写 warning 日志，并禁止以下会导致内容或状态不一致的动作：

- 访问公网以掩盖本地 storage 错误；
- 消费未确认 complete 的对象；
- 发布固化根或更新 tag/fallback index；
- 推进或执行无法持久化的删除；
- 盲目覆盖、删除或重试未知状态的对象。

## 10. 恢复和一致性测试范围

实现必须覆盖：

- 并发 PutIfAbsent、PutIfMatch 和 DeleteIfMatch；
- lease 丢失、接管、续租失败和 fencing；
- 进程崩溃留下 staging；
- S3 multipart 或条件操作结果未知；
- 文件系统 fsync/rename 后重启；
- 弱一致性 List；
- 损坏 data、metadata、complete 和 schemaVersion；
- 条件删除与新引用竞争；
- worker 重启后从 state、graph 和 checkpoint 恢复。
