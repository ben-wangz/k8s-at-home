# 09. 实现参考

## 1. crproxy

crproxy 是 mirror 能力的重点源码和行为参考，当前本地 clone 位于：

    /home/coder/temp/crproxy/

当前参考版本为 v0.12.6，commit 为 d6755ed，上游仓库为：

https://github.com/DaoCloud/crproxy

后续实现应重点研究：

- Registry HTTP 路径解析、上游 endpoint 和 Docker Hub 传输行为；
- Registry 认证 challenge、Bearer token 和 scope；
- manifest、blob 的缓存、digest 校验和并发下载协调；
- 文件系统与 S3/OSS/OBS 类 storage driver 的抽象；
- HTTP/HTTPS proxy、限速、重试、重定向和上游错误；
- mirror 请求的访问控制和管理接口。

crproxy 不作为 registry-hub 的 binary、独立服务、容器或 Go module 依赖。registry-hub 必须使用自己的 Go backend 重写 mirror 流量，并实现本地 push、标准 Distribution 后端、通用 OCI artifact 和异步固化。

## 2. OCI 和 Distribution

- [OCI Image Manifest Specification](https://github.com/opencontainers/image-spec/blob/main/manifest.md)：manifest、index、artifact type、subject、descriptor、config/layer media type 和扩展字段。
- [OCI Distribution Specification](https://github.com/opencontainers/distribution-spec/blob/main/spec.md)：Registry API、Referrers API、分页、digest-derived referrers tag fallback 和响应语义。
- [Distribution HTTP API V2](https://distribution.github.io/distribution/spec/api/)：manifest digest 删除、可选 tag 删除、blob API、tags/list 和标准错误语义。
- [Docker Hub Registry API](https://docs.docker.com/reference/api/registry/latest/)：Docker Hub 的实际 Registry endpoint `registry-1.docker.io`、Bearer challenge 和 pull API 示例；不能把 `docker.io` 逻辑名称误当作传输 endpoint。
- [Distribution reference grammar](https://github.com/distribution/reference/blob/main/regexp.go)：当前 `NameRegexp` 的可选 domain/port 前缀、ordinary remote-name component、tag 和 digest 语法；registry-hub 的 I(R) 显式不使用 domain 前缀。
- [Distribution configuration](https://github.com/distribution/distribution/blob/main/docs/content/about/configuration.md)：delete、readonly、upload purging、storage redirect 和 manifest/index validation 配置；GC 与通用 OCI artifact 的 chart 配置必须和 backend 约束一致。
- [OCI Distribution Conformance](https://github.com/opencontainers/distribution-spec/blob/main/conformance/README.md)：artifact、subject、referrer、descriptor data 等能力的测试范围。
- [Distribution Garbage Collection](https://distribution.github.io/distribution/about/garbage-collection/)：Distribution 的可达性标记、清理和 GC 期间写入约束。
- [Distribution tag deletion security advisory](https://github.com/distribution/distribution/security/advisories/GHSA-6pjf-3r9x-m592)：旧版本 tag 删除绕过 `storage.delete.enabled` 的安全问题；部署时必须使用包含修复的稳定版本。

实现必须以标准 Registry API 为 backend 与 Distribution 的内容边界。Distribution 私有 storage layout、driver、link 文件和私有 subject/GC API 不是业务接口。

## 3. ORAS

- [ORAS Pushing and Pulling](https://oras.land/docs/how_to_guides/pushing_and_pulling/)：任意文件、custom artifact type、custom layer media type 和多 layer artifact。
- [ORAS Multi-architecture Artifacts](https://oras.land/docs/how_to_guides/multiarch/)：多架构 binary manifest/index 的组织方式和平台选择行为。
- [ORAS Reference Types](https://oras.land/docs/concepts/reftypes/)：subject、referrer、discover 和关联 artifact 的客户端行为。

ORAS 发布的任意 binary 在 registry-hub 中都是普通 OCI manifest/index 加 blob layer；不执行、解包或转换 binary。多架构 index 的所有平台都属于固化闭包。

## 4. Helm OCI

- [Helm OCI Registries](https://docs.helm.sh/docs/v3/topics/registries/)：Helm config、chart content layer、provenance layer、tag 和 OCI 客户端约束。

Helm OCI 使用通用 OCI 路径处理；backend 不生成 index.yaml、不渲染 chart、不改写 Helm config、chart content 或 provenance layer。
