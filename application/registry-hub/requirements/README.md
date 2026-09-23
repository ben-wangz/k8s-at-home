# registry-hub 需求索引

本目录是 registry-hub 的唯一权威需求文档。原来的
application/registry-hub/requirement.md 已拆分并移除，不再保留第二份需要同步维护的文档。

## 阅读顺序

| 文件 | 内容 |
| --- | --- |
| [00-overview.md](00-overview.md) | 背景、目标、范围、架构和术语 |
| [01-routing-and-registry-api.md](01-routing-and-registry-api.md) | 路径路由、Registry V2 API、请求代理边界和 Referrers |
| [02-artifacts-and-solidification.md](02-artifacts-and-solidification.md) | OCI artifact 范围、内容图、异步固化、并发和本地 push |
| [03-storage.md](03-storage.md) | FileSystem/S3 抽象、任务状态、临时缓存、lease 和生命周期 |
| [04-upstream-mirror.md](04-upstream-mirror.md) | 公网 Registry 访问、认证、代理、重定向、缓存和重试 |
| [05-delete-and-gc.md](05-delete-and-gc.md) | 删除、generation/tombstone、GC 写入闸门和执行器 |
| [06-auth-security-and-limits.md](06-auth-security-and-limits.md) | 客户端/内部/上游认证隔离、安全边界和资源限制 |
| [07-operations-deployment-and-observability.md](07-operations-deployment-and-observability.md) | Helm 拓扑、配置、探针、管理面和可观测性 |
| [08-acceptance.md](08-acceptance.md) | 按模块整理的验收清单和关键故障场景 |
| [09-references.md](09-references.md) | crproxy、OCI、ORAS、Helm 和 Distribution 参考资料 |

## 已确定的总原则

- backend 是自行实现的 Go 二进制；不依赖 crproxy 的运行时、容器或 Go module。
- 对普通 manifest/blob 读取先查询内置 Distribution；只有明确的本地 404 才允许按路径访问公网。tags/list 和 Referrers 的回退例外以 01-routing-and-registry-api.md 为准。
- 写入、删除、上传 session 和 tag 操作只能进入内置 Distribution，绝不转发到公网 Registry。
- 第一段路径始终按字面解析为 upstream-host，不使用预配置 upstream 列表；没有 / 的仓库名不能回源。host:port 等逻辑路径保留原样，发往 Distribution 时通过可逆 I(R) 映射满足其仓库名语法。
- 内置 Distribution 是标准 Registry 后端。backend 不读取其存储布局、私有文件或私有管理接口；内容操作全部使用标准 Registry HTTP API。
- 公网获取的内容先进入抽象临时存储，再由异步任务通过标准 Registry API 固化；多架构 index 固化全部平台。
- 临时缓存、任务状态、引用图和 lease 使用统一 storage 抽象，必须支持 FileSystem 和 S3 实现。
- backend 和 Distribution 分离部署；两者均按单实例约束设计。Helm/Kubernetes 负责启动顺序、探针、升级和故障重启。
- 客户端、内部 Distribution、上游 Registry 三个认证域完全隔离；客户端凭据不得转发给公网。

## 文档维护约定

各规则只在一个模块中拥有完整定义，其他模块只引用其职责边界。必须、不得和明确的默认值属于实现约束；本目录不再保留旧的未决问题清单。实现阶段如果发现新歧义，应在对应模块补充决策并同步更新验收清单。
