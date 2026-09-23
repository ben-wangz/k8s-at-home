# 02. Artifact 范围与异步固化

## 1. Artifact 范围

registry-hub 按 OCI Distribution 的通用对象模型处理内容，不设置业务层面的 artifact media type 白名单。以下内容必须使用同一套 manifest/index/blob 流程：

- Docker Schema 2 manifest 和 manifest list；
- OCI image manifest 和 image index；
- Helm OCI chart、chart provenance 和 Helm config；
- ORAS binary、任意文件、多 layer artifact 和 custom artifact type；
- SBOM、签名、provenance、Wasm 以及其他结构合法的 OCI artifact。

backend 不解压、执行、渲染、扫描或解释 artifact 内容。未知 config、layer、artifact type、annotation 和 manifest 扩展字段，只要结构合法、资源受控并能通过标准 Registry API 保存，就必须透明传递和保存。

## 2. 原始内容保留

- manifest/index 的原始 bytes、media type 和 digest 必须保持不变。backend 只为遍历、校验和限制而解析，不重新序列化、排序、补字段、删除字段或改写 annotation。
- 必须保留 artifactType、config/layer media type、annotations、platform、subject、descriptor 扩展字段以及 descriptor.data。
- descriptor.urls 原样保留，但永远不访问其中的 URL，也不作为上游 blob fallback。内容闭包只能通过标准 Registry blob API 获取。
- descriptor.data 存在时，先 Base64 解码，再校验解码 bytes 的 size、digest 和资源限制。合法 bytes 可以直接作为 mirror/固化/本地 push pre-seed 来源；如果 Distribution 要求 blob 已存在，只能通过标准 blob API 上传完全相同的 bytes。
- descriptor.data 校验失败时，manifest/index 无效，不创建 complete 对象、固化任务或根对象。
- ORAS 常见的 application/octet-stream 或发布方自定义 layer media type 按普通 blob 保存；org.opencontainers.image.title 等文件名 annotation 必须保留，但 backend 不强制添加。

## 3. 内容闭包

固化根 artifact 的闭包规则如下：

| 根对象 | 必须固化的对象 |
| --- | --- |
| Docker Schema 2 或 OCI image manifest | 根 manifest；config descriptor 对应 blob；所有 layer descriptor 对应 blob |
| Docker manifest list 或 OCI image index | 根 index；所有子 manifest/index，递归处理；每个最终 manifest 的 config 和全部 layer |
| 任意直接按 digest/tag 请求的合法 manifest/index | 将该对象作为独立新根处理，不因 media type、artifact type 或内容未知而跳过 |
| Referrers API 返回的 discovery 响应 | 发现响应本身不自动作为根；只有客户端随后明确请求某个 referrer manifest/index 时才固化其闭包 |

多架构 index 必须固化全部平台，不按当前客户端的 Accept、platform 或命令行选择裁剪。多架构 binary 与多架构 image 规则相同：每个平台的子 manifest 都是闭包的一部分。

普通 manifest 的 config/layer 是对象依赖；subject 和 referrer 关系只保留关联，不自动递归。带 `subject` 的合法 manifest 即使 subject 尚未存在，也不能因此被 backend 拒绝；native/fallback referrer 维护按 01-routing-and-registry-api.md 处理。index/list 的子 manifest/index 才进入递归遍历。descriptor.data 如果对应合法且可递归的 manifest/index，先校验其 bytes，再按相同规则遍历。

## 4. 内容图校验

### 4.1 去重和引用边

单个任务和可复用缓存按 algorithm:digest 去重获取、校验和上传。每条引用边仍必须独立验证声明的 size、digest、mediaType、platform 和 annotations；不能因为同 digest 已存在就跳过边级别校验。

相同 digest 的声明 size 不一致、实际内容与 digest 不一致、递归类型不一致、manifest/index 结构非法或超出资源限制时，整个根对象失败。失败对象不得作为成功响应、Distribution 内容或新的任务 checkpoint。

### 4.2 循环检测

遍历使用活跃 DFS 路径集合：

- 相同 digest 在当前活跃路径之外再次出现，是共享引用，可以复用已验证对象；
- 相同 digest 在当前活跃路径中再次出现，是永久 manifest validation error；
- 循环、递归类型冲突和资源限制错误不自动重试，不发布根对象，不新建不完整根缓存；
- 已验证的独立共享对象可以由其他任务复用，但不能绕过当前引用边校验。

资源限制至少包括 manifest/index 大小、单任务对象数、总字节数和最大递归深度，默认值见 06-auth-security-and-limits.md。

## 5. Manifest discovery 与当前响应

公网 manifest/index 的 Accept 协商规则由 01-routing-and-registry-api.md 定义。固化相关约束如下：

1. discovery 获得合法完整 index 时，固化根必须是该 index，并包括全部平台。
2. 客户端不接受 discovery index 时，兼容交付请求只影响当前响应；兼容请求失败不阻止已验证 discovery 根进入后台固化。
3. 没有可接受 media type 返回 406，不保存错误缓存，不创建错误任务。
4. digest 请求不得用另一个对象、另一个 media type 或平台子对象替代。
5. HEAD 不产生缓存、任务或 blob body 副作用。

当前响应可以在本次请求涉及的对象完整写入临时 storage 并通过 digest/size 校验后返回；不需要等待整个闭包固化。

## 6. 固化任务模型

### 6.1 任务身份和状态

每个任务必须持久化以下信息：任务 ID、规范化 upstream authority、逻辑 repository R、根 digest/media type、发现方式、请求 reference、当前 generation、引用图版本、已完成对象 checkpoint、retryCount、nextAttemptAt、创建/更新时间和错误分类。任务状态至少包括：

任务和引用图中保存的 repository 始终是外部逻辑 R；访问内置 Distribution 时由 backend 即时计算 I(R)，不把内部编码名称写成对外身份。

- pending：已创建但尚未开始；
- running：有 worker 持有任务 lease；
- retry_wait：暂时性错误，等待 nextAttemptAt；
- solidified：根和完整闭包已导入并验证；
- failed：永久校验/安全/格式错误；
- cancelled：管理员取消且未发布根对象；
- repair_pending：Distribution 变更已部分完成，需要修复关联数据。

cleanup_pending 不是固化任务状态，而是已 solidified 任务关联的临时 cache 对象生命周期状态；任务保持 solidified，缓存对象按 03-storage.md 的宽限期和条件清理规则处理。

状态、引用图和 checkpoint 通过抽象 storage 持久化，是恢复时的唯一事实来源。内存队列只能作为唤醒优化，重启后从持久化状态重建。

### 6.2 入队和调度

对会触发 artifact 固化的公网 manifest/index GET，必须先持久化任务状态并预留待处理配额，成功后才允许访问公网。队列满、任务状态无法持久化或 storage 不可用时，返回 503 和 Retry-After，不访问公网。相同根任务复用已有任务和配额；内置 Distribution 命中不受固化队列限制。HEAD、tags/list、Referrers discovery 和仅 blob GET 不单独创建固化任务，但仍遵守对象缓存、校验、并发和错误规则。

全局最多 4 个 solidification worker。新任务按规范化 upstream 轮询；repair_pending 优先，但必须保留 1 个槽位给新固化任务，无修复任务时可借用。只调度已持久化且到达 nextAttemptAt 的任务。

任务按拓扑顺序导入内置 Distribution：先验证并准备 blob，再上传 manifest，再上传依赖它的 index，最后发布根 reference/tag。每个阶段都必须通过标准 Registry API 的读取验证；不能用“上传请求成功”代替内容可读性验证。

### 6.3 成功和清理

只有根对象、全部闭包对象和根 reference 经 Distribution 标准 API 验证可读后，任务才能进入 solidified。成功后缓存按 03-storage.md 的宽限期进入 cleanup_pending；清理失败不回滚已成功的 pull 或 Distribution 内容。

共享对象不能使用简单引用计数决定删除。清理前必须重新检查 complete、任务引用图、active lease、对象版本和 generation；任何新引用或状态变化都放弃本次清理。

## 7. 公网对象并发下载

### 7.1 对象 lease

同一规范化 upstream authority、逻辑 repository R 和 digest 的下载使用对象级 lease：

- key 为 authority + 逻辑 repository R + digest 的稳定 hash，存放在 leases/objects/；
- lease 默认 TTL 60 秒，持有者每 20 秒续租，使用 fencing token 写入 checkpoint；
- leader 负责公网下载、Range checkpoint、digest/size 验证和 complete 提交；
- waiter 不重复公网下载，只等待 complete 或 leader 的最终错误；
- leader lease 到期只能被一个新 leader 接管；旧 leader 的写入因 fencing token 失效；
- waiter 最多等待 30 秒，超时返回 503 和 Retry-After；客户端重试可以再次加入同一 lease；
- 下载 checkpoint 必须绑定 authority、repository、digest、声明 size、Range/offset 和上游响应身份，不能把不同对象的断点拼接。

leader 完成 complete 后，所有 waiter 从临时 storage 读取验证过的对象响应。对象错误不能写成 complete，也不能由 waiter 重新绕过 lease 访问公网。

### 7.2 请求内 dedupe

tag 到 digest 的映射只存在于当前请求和 in-flight 去重窗口，不写入持久化 tag 映射；规范化 Accept 变体需要分别处理。内置 Distribution 命中后不进行公网 refresh。

## 8. 本地 push 与 mirror 固化竞争

本地 push 和 mirror 固化不分命名空间。相同 tag 的根发布以先完成并验证的一方为准：

- 同 digest 的重复 PUT 是幂等成功；
- 不同 digest 的后完成者返回 409 Conflict，不覆盖已发布 tag；
- backend 必须在根 manifest/index、tag 或 fallback index 发布前检查 generation、取消标记和写入 gate；
- 已经上传但未成为根引用的 blob 不回滚，由 Distribution GC 或正常清理处理；
- push、固化和 fallback referrer 维护使用不同粒度的 lease，不能用全局锁阻塞无关仓库。

## 9. 取消、删除和恢复

管理员取消通过任务状态条件写入和 fencing token 生效。worker 在阶段边界以及发布根 manifest/index、tag 或 fallback index 前检查取消。取消不回滚已经上传的对象，旧 worker 不得继续发布；如果根发布先完成，则取消返回 409。repair_pending 不允许取消。

删除先推进抽象 storage 的 deletion generation/tombstone，再执行 Distribution 删除。旧任务在根、tag 或 fallback index 发布前检测 generation 变化并停止。generation、tombstone、宽限期和 GC 关系由 05-delete-and-gc.md 定义。

## 10. Helm OCI 与 ORAS 约束

Helm OCI 不采用专用代码路径，不生成 index.yaml、不渲染 chart、不改写 Helm config、chart content 或 provenance layer。Helm 的 chart 名称、SemVer tag 和客户端选择行为由 Helm 客户端负责。

ORAS 发布的文件表现为普通 manifest 加 blob layer；backend 只保存和交付 OCI 描述的原始 bytes、media type、digest、size、platform、subject 和 annotations。多架构 binary 的所有平台必须进入同一个固化闭包。
