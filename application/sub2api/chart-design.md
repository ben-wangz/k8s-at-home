# sub2api Helm Chart Design

## 1) 目标

在 `application/sub2api/chart/` 设计一个可维护、可扩展的 Helm Chart，用于部署 `sub2api`，并满足：

- 参考 `application/podman-in-container/chart/` 的写法，充分复用 Bitnami `common`。
- 基础镜像使用 `ghcr.io/wei-shaw/sub2api:0.1.111`。
- 支持依赖模式切换：
  - `redis.enabled` / `postgresql.enabled`（内置子 chart）
  - `externalRedis` / `externalPostgresql`（外部服务）
- 默认开启内置 Redis + PostgreSQL，并自动生成默认连接参数。
- 模板渲染支持 `common.tplvalues.render`，允许像 Bitnami Kibana 那样在 values 中使用 `{{ include ... }}` 风格模板值。

---

## 2) 源码与部署分析结论

### 2.1 存储与依赖结论（确认项）

基于 `build/sub2api` 源码和 compose 文件分析：

1. **PostgreSQL 是核心持久化存储（确认）**
   - 业务模型与迁移均在 PostgreSQL（Ent + SQL migrations）。
   - 参考：
     - `build/sub2api/backend/internal/repository/ent.go`
     - `build/sub2api/backend/internal/setup/setup.go`
     - `build/sub2api/deploy/docker-compose.yml`

2. **Redis 主要用于缓存/并发控制/队列辅助（确认）**
   - 代码中 Redis 仓储基本为 `*cache`，如 `billing_cache`、`concurrency_cache`、`dashboard_cache` 等。
   - 参考：
     - `build/sub2api/backend/internal/repository/redis.go`
     - `build/sub2api/backend/internal/repository/billing_cache.go`
     - `build/sub2api/backend/internal/repository/concurrency_cache.go`
     - `build/sub2api/backend/internal/repository/dashboard_cache.go`

3. **sub2api 主服务“业务数据层面”可横向扩展，但容器内仍有本地文件状态（部分确认）**
   - 业务主数据在 PostgreSQL，可多副本运行。
   - 但本地会使用 `/app/data` 保存：
     - `config.yaml`、`.installed`（自动初始化流程）
     - 日志文件默认路径 `/app/data/logs/sub2api.log`
     - pricing 数据文件缓存目录（`pricing.data_dir`）
   - 参考：
     - `build/sub2api/backend/internal/setup/setup.go`
     - `build/sub2api/backend/internal/config/config.go`
     - `build/sub2api/backend/internal/pkg/logger/options.go`
     - `build/sub2api/backend/internal/service/pricing_service.go`
     - `build/sub2api/Dockerfile`

结论：
- **数据库层面**：PostgreSQL 必须持久化；Redis可视为高价值缓存/协调层，理论可重建。
- **应用容器层面**：建议保留 `/app/data` 持久化选项（默认开），以避免重启后反复 auto-setup、日志/本地缓存丢失。

---

## 3) Chart 总体结构设计

### 3.1 目录结构（规划）

```text
application/sub2api/chart/
  Chart.yaml
  values.yaml
  templates/
    _helpers.tpl
    deployment.yaml
    service.yaml
    ingress.yaml
    pvc.yaml
    secret.yaml
    extra-list.yaml
```

### 3.2 Chart 依赖

`Chart.yaml` 增加依赖：

- `common`（bitnami-common）
- `redis`（bitnami/redis）
- `postgresql`（bitnami/postgresql）

并设置条件：

- `condition: redis.enabled`
- `condition: postgresql.enabled`

---

## 4) values 设计（核心）

### 4.1 基础运行配置

- `replicas`
- `image.repository/tag/pullPolicy`（默认 `ghcr.io/wei-shaw/sub2api:0.1.111`）
- `resourcesPreset/resources`
- `podSecurityContext/containerSecurityContext`
- `nodeSelector/tolerations/affinity`
- `service` / `ingress`
- `extraEnvVars` / `extraEnvVarsSecret` / `extraVolumes` / `extraVolumeMounts` / `extraDeploy`

### 4.2 应用配置分组

- `sub2api.autoSetup`（默认 `true`）
- `sub2api.server`（host/port/mode/runMode/timezone）
- `sub2api.auth`（adminEmail/adminPassword、jwtSecret、totpKey）
- `sub2api.database`（maxOpen/maxIdle/lifetime/idleTime/sslmode）
- `sub2api.redis`（db/pool/minIdle/enableTLS）
- `sub2api.env`（透传高级环境变量 map，支持 tpl）

### 4.3 存储配置

- `persistence.data.enabled`（默认 `true`）
- `persistence.data.size/storageClass/existingClaim/...`
- mount 到 `/app/data`

### 4.4 依赖配置结构（满足你的要求）

```yaml
redis:
  enabled: true
  architecture: standalone
  auth:
    enabled: true
    password: ""
    existingSecret: ""
  master:
    persistence:
      enabled: true

postgresql:
  enabled: true
  auth:
    username: sub2api
    password: ""
    database: sub2api
    existingSecret: ""
  primary:
    persistence:
      enabled: true

externalRedis:
  host: ""
  port: 6379
  password: ""
  existingSecret: ""
  existingSecretPasswordKey: redis-password
  db: 0
  enableTLS: false

externalPostgresql:
  host: ""
  port: 5432
  username: sub2api
  database: sub2api
  password: ""
  existingSecret: ""
  existingSecretPasswordKey: postgres-password
  sslmode: disable
```

---

## 5) 连接参数生成策略（内置/外部自动切换）

在 `_helpers.tpl` 提供统一 helper：

- `sub2api.redis.host`
- `sub2api.redis.port`
- `sub2api.redis.password`
- `sub2api.postgresql.host`
- `sub2api.postgresql.port`
- `sub2api.postgresql.username`
- `sub2api.postgresql.database`
- `sub2api.postgresql.password`

逻辑：

1. 若 `redis.enabled=true`：使用依赖子 chart service 名。
2. 若 `redis.enabled=false`：使用 `externalRedis.*`。
3. PostgreSQL 同理。

并在 `deployment.yaml` 中把环境变量统一映射为：

- `DATABASE_HOST/PORT/USER/PASSWORD/DBNAME/SSLMODE`
- `REDIS_HOST/PORT/PASSWORD/DB/ENABLE_TLS`

---

## 6) Bitnami common + tpl 渲染策略

为了做到你要求的 Kibana 风格灵活性：

1. 对 `extraEnvVars`、`extraVolumes`、`extraVolumeMounts`、`podAnnotations`、`commonAnnotations` 等，统一使用：

```gotmpl
{{- include "common.tplvalues.render" (dict "value" .Values.xxx "context" $) }}
```

2. 在 values 中允许模板字符串（例如把外部地址写成 include）。

3. 提供示例（文档中给出）：

```yaml
sub2api:
  env:
    DATABASE_HOST: '{{ include "sub2api.postgresql.host" . }}'
    DATABASE_PORT: '{{ include "sub2api.postgresql.port" . }}'
    REDIS_HOST: '{{ include "sub2api.redis.host" . }}'
    REDIS_PORT: '{{ include "sub2api.redis.port" . }}'
```

---

## 7) 与 docker-compose 的映射策略

优先覆盖 compose 里关键环境变量：

- 自动初始化：`AUTO_SETUP`
- 服务：`SERVER_HOST/SERVER_PORT/SERVER_MODE/RUN_MODE/TZ`
- DB：`DATABASE_*`
- Redis：`REDIS_*`
- 管理员：`ADMIN_EMAIL/ADMIN_PASSWORD`
- 安全：`JWT_SECRET/TOTP_ENCRYPTION_KEY`
- 可选增强：`GEMINI_*`、`UPDATE_PROXY_URL`、`SECURITY_URL_ALLOWLIST_*` 等通过 `sub2api.env` 扩展

健康检查默认对齐 compose：`GET /health`（HTTP 200 即 healthy）。

---

## 8) 关键风险与处理

1. **多副本 + AUTO_SETUP 并发初始化**
   - 默认建议 `replicas: 1` 起步。
   - 若多副本，要求固定 `JWT_SECRET` / `TOTP_ENCRYPTION_KEY`，避免各 pod 随机生成不一致。

2. **Redis 可用性风险**
   - Redis 虽偏缓存/协调层，但服务运行期大量功能依赖 Redis 访问；生产建议仍使用持久化与高可用方案。

3. **/app/data 语义**
   - 即使业务主数据不在本地，仍建议提供 PVC 默认开启，避免每次重启都重新 auto-setup 与本地缓存冷启动。

---

## 9) 实施顺序（下一步）

1. 搭建 `Chart.yaml` 与依赖声明（common/redis/postgresql）。
2. 落地 `values.yaml`（含依赖与 external 结构）。
3. 编写 `_helpers.tpl`（连接选择逻辑 + secret 命名）。
4. 编写 `deployment.yaml`（环境变量注入、探针、volume、tpl 渲染）。
5. 编写 `service.yaml`、`ingress.yaml`、`pvc.yaml`、`secret.yaml`、`extra-list.yaml`。
6. `helm template` 验证以下场景：
   - 默认内置 Redis/PostgreSQL
   - 关闭内置并切换 externalRedis/externalPostgresql
   - `existingSecret` 与明文值两种密码输入
