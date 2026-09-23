# 05. 删除与 Distribution GC

## 1. 删除边界

删除请求只操作内置 Distribution，永远不发送给公网 Registry。backend 仍须遵守 01-routing-and-registry-api.md 的标准 Registry API 边界，不读取或修改 Distribution 私有 storage layout。

删除规则：

- manifest digest 删除按 Distribution/OCI 标准返回 `202 Accepted`；不会因为仍有其他 tag 指向该 digest 而额外拒绝。对内置 Distribution 的验证读取中，该 digest 及指向它的 tag 的 manifest GET 应返回 404，backend 不额外发起 tag 删除或 blob 删除；保留的 fallback referrers tag 是受保护例外，覆盖/删除及其当前 index manifest 的 digest 删除返回 409；
- manifest tag reference 删除是 OCI 可选的标准 tag deletion：Distribution 删除启用时成功返回 `202 Accepted`，目标不存在返回 `404`；删除被禁用时保留标准 `400`/`405`。backend 不把 tag reference 转换成 digest 删除；digest PUT 的 `tag` 参数同样遵守该保护规则；
- 删除 manifest 不级联删除 blob、config、layer、subject 或无关 referrer。删除一个 referrer manifest 时，fallback index 的关联 descriptor 按 01-routing-and-registry-api.md 的维护流程更新；native referrers 由 Distribution 维护；
- blob DELETE 按 Distribution 标准 API 执行，成功返回 `202 Accepted`，不存在返回 `404`。该操作只删除内置 Distribution 的 blob，调用方必须承担删除被 manifest 引用的 blob 后内容不可解析的标准后果；GC 仍负责回收其他不可达 blob；
- 目标不存在返回 404，不回源；
- 删除期间遇到 GC 写入闸门、storage 或 Distribution 故障，返回明确错误，不把部分成功伪装成不存在。

Distribution 的标准 API 支持按 digest 删除 manifest、按 digest 删除 blob，以及在启用时按 tag 删除 tag；registry-hub 不通过私有 storage layout 实现任何删除，也不把普通 tag reference 转换成 digest 删除。Distribution 必须启用删除能力以满足 registry-hub 的删除需求；backend 的认证、generation 和 GC 闸门仍可拒绝具体请求。fallback referrers 保留 tag 的外部覆盖/删除仍按本模块的 409 保护规则处理。

任何删除成功都必须经过内置 Distribution 标准 API 的响应和必要的内部 GET 验证；该验证禁止触发公网 fallback，避免删除后的本地 404 被重新解释为公网命中。删除是本地内容操作，不向公网创建删除请求或永久屏蔽标记；后续普通外部 pull 仍按 01-routing-and-registry-api.md 的 local-first 规则执行，必要时可以重新从公网返回同一路径的内容。backend 不通过删除 filesystem link、S3 key 或其他内部对象实现 Registry 删除。

## 2. Deletion generation 与 tombstone

删除和异步固化可能并发，必须在执行 Distribution 删除前推进 storage 中的 deletion generation，并写入 tombstone：

1. 按 repository/reference 获取相应 lease；
2. 条件递增 generation；
3. 持久化 tombstone、删除目标和期望状态；
4. 让旧固化任务在发布根 manifest/index、tag 或 fallback index 前检查 generation；
5. generation 未变化时才执行标准 Registry DELETE；
6. DELETE 后重新读取确认，并更新任务/墓碑状态。

旧任务检测到 generation 变化必须停止，删除不存在的目标也必须阻断旧任务后续发布。新的本地 push 或 mirror 任务建立新的 generation 关联。

generation counter 永久保留，tombstone 可清理但必须同时满足：

- 没有 active、retryable 或 repair_pending 任务引用；
- 相关 lease 已释放或过期；
- 默认安全宽限期 24 小时已完成；
- 条件删除前后 generation 未变化。

清理失败只延迟回收，不恢复旧任务，也不影响已经完成的 Registry 删除。

## 3. GC 目标和默认策略

内置 Registry 使用部署中选定并固定版本或 digest 的 Distribution 官方稳定版本镜像。backend 不实现 GC 算法，不解析 Distribution storage，不自行寻找可达 blob。GC 只使用 Distribution 官方的 GC executable 或与其版本、配置、storage 完全一致的 Job。

默认 GC 策略：

- GC interval 默认关闭，使用 0 表示不自动调度；
- 通过受保护管理接口显式触发、查询、重试、恢复；
- 默认不启用 delete-untagged，除非明确的部署配置开启；
- GC 是 Distribution 官方的 stop-the-world mark/sweep 操作。在 registry-hub 中，backend 写入闸门和“唯一写入口”证明是强制前提：NetworkPolicy/Service 只允许 backend 访问 Distribution；backend 为单实例；所有已接纳的客户端写入、upload session、后台固化和 repair 写入已排空或取消；`maintenance.uploadpurging` 等后台 storage writer 已关闭；不存在其他访问同一 storage 的 Registry 进程。满足这些条件后，Distribution 可以继续提供只读请求，独立 GC Job/进程直接执行官方 GC。以 `maintenance.readonly.enabled=true` 配置重启/替换 Distribution 或完全停止它是可选的纵深防御；如果无法证明没有其他 storage writer，则必须启用该防御或拒绝启动 GC；
- GC 的执行身份、版本、配置摘要、storage 目标、开始/结束时间、状态和结果必须持久化。

## 4. 写入闸门

写入闸门状态为：

    open -> draining -> backend_quiesced -> gc_running -> restore -> open
                                               \-> failed/unknown -> recover -> open

- open：允许正常写入；
- draining：拒绝新写入，等待已经接纳的客户端写入、upload session、后台固化发布和 repair 写入在默认 5 分钟窗口内完成；
- backend_quiesced：backend 写入闸门已持久化关闭，已接纳的写入和会改变 Distribution 内容的 worker 已排空或取消，并已确认 backend 是唯一写入口、后台 writer 已关闭；Distribution 可以继续提供只读请求，也可以由可选生命周期适配器切换为只读或停止；
- gc_running：封禁所有写入并执行官方 GC；Distribution 继续运行时允许读取，若被可选生命周期适配器停止则读取返回依赖不可用；
- restore：GC 已报告成功；如果 GC 前启用了只读/停止防御，先恢复 Distribution，再重新验证可读性、配置和 storage 状态；
- failed/unknown：继续封禁写入，直到受保护的 retry/recover 操作确认实际 GC 状态。

闸门封禁范围包括客户端 push、manifest/index PUT/DELETE、blob upload 创建/分块/完成/取消、tag/fallback index 更新、repair 发布和其他会改变 Distribution 内容的 backend 操作。只要唯一写入口和无后台 writer 条件仍成立，GET、HEAD、tags/list、referrers read 和 GC 状态查询可以继续服务；如果 Distribution 被可选生命周期防御停止，这些请求返回依赖不可用，不能回源伪造结果。

新的写入在 draining 开始后立即返回 503 或明确的 GC in progress 错误，并带 Retry-After；已经进入 Distribution 的请求和已获得写入 admission 的后台发布允许完成，但不超过 drain deadline。新的固化/repair worker 阶段不得获得写入 admission。deadline 到达后，backend 必须完成 backend_quiesced 的唯一写入口和无后台 writer 证明；如果无法证明，则必须通过可选生命周期适配器将 Distribution 切换为只读或停止，不能在写入状态不确定时启动 GC。

只有执行器报告成功、必要的只读/停止防御已恢复（如果启用）、backend 重新确认 Distribution 可读、配置和 storage 状态，并且持久化状态提交成功后，才能回到 open。不能通过重启进程、删除锁文件或无条件设置 open 强制解锁。

## 5. 单机执行方式

单机部署时，backend 和 Distribution 是同一个 OS 内的两个不同进程。backend 先完成写入闸门、请求 drain、唯一写入口和后台 writer 检查，再使用配置的绝对路径启动官方 GC executable，并传入与当前 Distribution 完全一致的配置和 storage 目标。若部署选择启用只读/停止这一可选防御，backend 再调用受控的 Distribution 生命周期适配命令，并确认新进程已按 `maintenance.readonly.enabled=true` 配置运行或旧进程已完全停止后才启动 GC：

- 不使用相对路径；
- 不依赖当前工作目录；
- 不通过 shell 拼接用户输入；
- 记录不可泄露的执行身份和参数摘要，不记录 Secret；
- 使用独立进程组、context deadline、stdout/stderr 受控采集和退出状态；
- 进程退出状态未知时先读取 Distribution/GC 状态或执行受保护 reconcile，不能直接认为失败或成功。
- 没有可验证的唯一写入口和后台 writer 状态时拒绝启动 GC；如果选择了只读/停止防御，GC 成功后必须按配置重启/替换回可写 Distribution，或恢复已停止的进程，并重新验证。

backend 仍不读取 Distribution storage；GC executable 自己解释其配置和存储。

## 6. Kubernetes 执行方式

Kubernetes 环境使用独立的 Distribution GC Job，原因是 GC 是一次性、可能长时间运行并且需要与 backend 生命周期隔离的存储运维动作。Job 只负责执行和报告 GC，backend 的写入闸门与唯一写入口检查负责保证 GC 前没有并发 writer：

- backend 通过 Kubernetes API 创建并监控独立 Job，不使用 kubectl exec，也不把 GC 进程放进 backend container；
- Job 使用与当前内置 Distribution 相同版本的 Distribution 镜像、配置、Secret、registry storage 配置和 GC 参数；
- 创建 Job 前，backend 必须完成 draining，确认没有已接纳的写入、没有新的写入 admission、只有 backend 能访问 Distribution、upload purging 已关闭且没有其他 storage writer；满足这些条件后 Job 才能启动。若部署选择只读/停止防御，才通过受控生命周期适配重启/替换 Distribution 为 `maintenance.readonly.enabled=true`，或停止/缩容所有可写 Distribution Pod；read-only 模式下 backend 可以继续提供读取，停止模式下读取返回依赖不可用；
- Job 不创建 Service，不接收 Registry 流量；
- parallelism=1、completions=1、backoffLimit=0、restartPolicy=Never；
- Job 的失败、被删除、Pod 状态未知、registry storage 挂载/连接异常或版本/配置摘要不匹配，都使写入闸门保持封禁；
- Job 的 owner、name、UID、版本/配置/storage 摘要和最终状态持久化到任务状态；
- backend 不因为 API watch 断开就推断 Job 已完成；恢复后必须通过 API 重新读取 Job/Pod 状态；
- 只有明确成功的 terminal Job、必要的生命周期防御已恢复（如果启用）、Distribution 可读验证和状态条件写入完成后才解除闸门。

Job 只解决 GC 的执行隔离和可观测生命周期，不改变 backend 对 Distribution 标准 API 的内容访问边界，也不能替代 backend 的写入闸门。Distribution 的只读配置重启/替换或停止是无法证明唯一写入口时的可选防御。具体 RBAC、存储挂载或对象存储配置、Pod 调度和集群兼容性由 Helm chart/部署环境负责。

## 7. GC 故障和恢复

GC 执行失败、状态 unknown、版本不匹配、storage 错误或 backend 在关键状态写入时崩溃时：

1. 保留 gc_running 或 failed/unknown 状态；
2. 拒绝所有写入；
3. 保存执行器 identity 和最后一次已知状态；
4. 如果 Distribution 仍运行且唯一写入口条件保持成立，允许继续读取；如果启用了停止防御则读取返回依赖不可用；status/retry/recover 仍受保护；
5. retry 只有在确认没有另一个执行器运行、唯一写入口条件仍成立，或已启用只读/停止防御后才能再次启动；
6. recover 必须重新查询本机进程或 Kubernetes Job，并由管理员确认后恢复 open；
7. 禁止自动删除未知 Job、强制杀死未知 GC 或绕过闸门。

如果 Distribution GC 进程在 backend 状态写入前退出，必须通过执行器 identity、Job UID/进程信息和 Distribution 可读性进行 reconcile。无法证明 GC 已安全结束时，按 unknown 处理。

## 8. GC 与任务、Referrers 的关系

GC 闸门封禁期间，不能发布 solidification 根、更新 fallback referrer index、执行 repair_pending 或清理会改变 Distribution 内容的操作。临时 storage 的 cache/task 清理可以继续，但必须遵守 03-storage.md 的条件删除和 generation 检查。

GC 结束后，backend 不假设旧任务仍然有效。任务在恢复调度和发布前重新读取 state、graph、generation、Distribution 对象和 fallback index；缺失或不确定时进入 reconcile。
