# 00. 总览

## 1. 背景

现有基础组件中有两个相互独立的服务：

- container-registry：可写入的 Distribution Registry，用于保存本地 push 的镜像。
- container-registry-mirror：带代理缓存能力的 Registry，用于从 Docker Hub、GHCR 等远端 Registry 拉取内容。

registry-hub 统一提供这两类能力：客户端只有一个入口，既可以从内置 Registry 读取和 push，也可以在本地没有内容时按请求路径访问公网 Registry，并将验证过的 OCI 内容异步固化到内置 Registry。

## 2. 目标

1. 提供兼容 Docker Registry HTTP API、OCI Distribution API 的统一入口。
2. 支持本地 push、pull、tag、删除和上传 session。
3. 支持按请求路径指定任意公网 Registry，不依赖预配置 upstream 列表或 upstream ID。
4. 透明代理和固化 Docker image、OCI image/index、Helm OCI chart、ORAS binary、SBOM、签名、provenance、Wasm 以及其他合法 OCI artifact。
5. 在进程重启、Pod 重启或节点迁移后保留内置 Registry 内容、临时缓存和可恢复任务状态。
6. 在 Distribution GC 期间阻断所有写入；当 backend 已证明自己是唯一写入口且后台 writer 已关闭时，可以继续提供读取服务；如果 GC 选择停止 Distribution，则该期间读取返回依赖不可用。

## 3. 非目标

- 不把 crproxy 作为运行时依赖；只参考其 mirror 实现和行为。
- 不负责 Docker、containerd 或 Argo CD 客户端的实现；交付边界是标准 Registry/OCI HTTP API 和 Helm chart。
- 不在 registry-hub 内实现账号注册、密码修改、Token 签发或完整身份目录。
- 不根据客户端 platform 选择性固化多架构 index，不重写或渲染 artifact 内容。
- 不为集群特定的 Ingress、存储类、网络插件或高可用拓扑提供兼容层。
- 不直接依赖 Distribution 的 filesystem layout、storage driver、link 文件或私有管理 API。

## 4. 组件与拓扑

系统由两个独立进程组成：

下面是 Kubernetes/Helm 交付时的拓扑。单机部署不使用 Deployment/Service，而是在同一 OS 内运行 backend 和 Distribution 两个独立进程，同时保留 backend 是唯一内容访问入口、Distribution 不对外暴露的边界。

    External client
          |
       Ingress/TLS
          |
      Go backend  ---- private HTTPS Service ---->  Distribution Registry
          |                                             |
          +---- FileSystem or S3 temporary storage     +---- persistent Registry storage

- backend 是对外的统一入口，负责路由、上游访问、缓存、内容验证、异步固化、删除控制和 GC 写入闸门。
- Distribution 是内置内容后端，使用选定并固定版本或 digest 的官方稳定版本镜像；Kubernetes 中由 registry-hub 自己的 Helm chart 引用，不使用 Distribution Helm chart，单机模式由部署环境直接运行该镜像/进程。
- Kubernetes 中 backend 与 Distribution 使用分离的 Deployment/Service，不使用 sidecar。
- Kubernetes 中 Distribution 只暴露私有 Service，NetworkPolicy 只允许 backend 访问；不提供外部 Ingress、NodePort 或 LoadBalancer。
- Kubernetes 外部客户端访问 backend 的 Ingress；backend 不维护客户端侧 CA，Ingress 负责外部 TLS。单机入口由部署环境提供，backend 同样不实现客户端 CA 机制。
- backend 到 Distribution 使用私有 HTTPS Registry API。Distribution 的内部自签证书由部署环境提供，Kubernetes 中可通过 Helm chart 或 Secret 注入；backend 默认启用证书校验并信任该 CA；不使用 mTLS，也禁止通过跳过校验解决证书问题。
- backend 与 Distribution 均按单实例设计。Distribution 使用单写入的持久存储配置，可以是 FileSystem/PVC 或 Distribution 支持的对象存储；backend 不依赖其存储实现和布局。多副本和高可用不属于当前范围。

## 5. 核心流程

### 5.1 读取和回源

1. backend 解析 Registry V2 endpoint，得到完整外部逻辑仓库名 R，并计算仅供内部使用的 Distribution 仓库名 I(R)。
2. backend 使用 I(R) 先把同一个请求发送到内置 Distribution；客户端永远只看到 R。
3. 只有内置 Distribution 对当前对象返回明确的 404 时，才允许回源；其他状态码不能伪装成未命中。tags/list 和 Referrers 的具体回退例外由 01-routing-and-registry-api.md 定义。
4. R 含 / 时，第一段是字面 upstream-host，其余部分是上游镜像路径；R 不含 / 时不能回源。
5. backend 按客户端 Accept、上游认证、代理、TLS、重定向和 SSRF 规则访问公网。
6. 公网对象先通过 digest、size 和结构校验写入临时 storage。当前请求可以在本次响应对象完整验证后返回，不等待整个引用闭包固化。
7. backend 持久化固化任务，后台补齐根对象的完整内容闭包，并通过标准 Registry API 导入 Distribution。
8. Distribution 中的根对象及其全部引用对象经读取验证后，临时缓存才进入清理流程。

### 5.2 本地写入

所有 POST、PATCH、PUT、DELETE 只操作内置 Distribution。mirror 路径和本地 push 路径没有命名空间区分，因此相同仓库路径可以分别 push 和 pull；同一 tag 不同 digest 的竞争规则由 02-artifacts-and-solidification.md 定义。

### 5.3 GC

GC 由管理面触发，backend 先进入 draining，等待正在进行的客户端写入、后台固化发布和 repair 写入结束，再封禁所有 push/delete/upload session 写入，并确认 backend 是 Distribution 的唯一写入口、后台 writer 已关闭。满足这些条件后，可以在 Distribution 继续提供只读请求的情况下执行官方 GC；按官方只读配置重启/替换 Distribution 或停止它是可选的纵深防御，不能替代 backend 闸门。若无法证明没有其他 storage writer，则必须启用该防御或拒绝启动 GC。GC 失败、执行状态未知或恢复信息不确定时，写入保持封禁，直到受保护的恢复操作确认状态。

## 6. 术语

- 内置 Registry/Distribution：由 Helm 或单机部署提供、只允许 backend 访问的标准 Distribution 后端。
- 上游 Registry：由请求路径第一段指定的公网 Registry authority，例如 ghcr.io 或 host:port。
- 逻辑仓库名 R：/v2/ 后、明确 endpoint 前的完整路径，例如 docker.io/library/busybox。
- 内部仓库名 I(R)：backend 将 R 映射到不带 Distribution domain 前缀的 ordinary remote-name；host:port 等逻辑 authority 使用可逆编码，外部路由和存储身份仍使用 R。
- 根 artifact：本次 pull 或固化任务直接请求并验证的 manifest/index。
- 内容闭包：根 manifest/index 按 OCI 引用规则需要的所有 manifest、config 和 layer blob；index 必须包括全部平台。
- artifact 固化：把已验证的临时内容通过标准 Registry API 写入内置 Distribution，并确认完整闭包可读。
- complete 对象：已完成 data、metadata、digest/size 校验并通过提交标记确认的临时对象。
- 任务：持久化的异步固化、修复、清理或恢复工作单元。
- lease：storage 层提供的带 TTL 和 fencing token 的协调锁；不表示 backend 可以访问 Distribution 私有存储。
- generation/tombstone：删除与异步任务之间的永久 generation 计数和临时删除标记，用于阻断旧任务发布。

## 7. 设计不变量

1. local-first：内置 Distribution 的成功结果优先，只有明确本地 404 才可能回源。
2. write-local-only：公网只读；所有写、删、上传和 tag 操作在内置 Distribution 完成。
3. verify-before-use：临时对象、根对象和每条引用边都必须验证后才能返回、消费或发布。
4. standard-backend：backend 不绕过标准 Registry API 操作 Distribution 内容。
5. async-complete：当前 pull 可以先返回，但固化任务必须可恢复、可重试、可观测。
6. credential-isolation：客户端 Authorization、内部 Distribution 凭据和上游凭据永不跨域转发。
7. fail-closed：storage、GC 状态、权限和安全校验不确定时，不通过回源或写入来掩盖问题。

## 8. 交付和部署边界

配置从 backend 环境变量读取。Secret 或 workload identity 负责注入凭据；敏感值不写入 Helm values、日志或任务状态。Helm chart 负责 backend/Distribution 的启动顺序、探针、Service、Secret 挂载、按存储模式提供 PVC 或对象存储配置、更新策略和故障重启。项目不为某个具体集群的部署兼容性增加额外协议。
