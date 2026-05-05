# LiteLLM 在 ECS(k3s+ArgoCD) 的部署与验证（可复现流程）

本文档用于让另一个 coding agent 在新环境中稳定复现：

- 使用 ArgoCD 部署 LiteLLM
- 使用集群内 OCI registry 作为 chart 源
- 使用 `m.daocloud.io` 镜像策略
- 验证 Helm 模板修复后，standalone DB 不再发生 secret 漂移

配套文件：`ecs/litellm/litellm.app.yaml`

---

## 0) 结论先行（本次修复验证重点）

目标不是验证业务模型配置，而是验证 Helm chart secret 行为：

- `db.deployStandalone=true`
- `db.dbCredentialsSecretName=litellm-credentials`
- `postgresql.auth.existingSecret=litellm-credentials`

验证通过标志：

1. `litellm-postgresql-0` / `litellm-redis-master-0` / `litellm-*` 全部 `Running`
2. `kubectl get secret -n agents litellm-dbcredentials` 不存在
3. LiteLLM 日志无 `P1000 Authentication failed`

---

## 1) 前置条件

请先完成：`ecs/k3s-argocd-with-daocloud-mirror.md`

并确保：

- k3s 与 ArgoCD 已就绪
- 可以 SSH 到目标 ECS 主机
- `helm` 在本地与 ECS 主机上可用

---

## 2) 在集群内部署 OCI registry（chart 源）

```bash
kubectl create namespace registry --dry-run=client -o yaml | kubectl apply -f -

kubectl create deployment registry -n registry --image=registry:2 --port=5000 --dry-run=client -o yaml | kubectl apply -f -

kubectl expose deployment registry -n registry --type=NodePort --port=5000 --target-port=5000 --dry-run=client -o yaml | kubectl apply -f -

kubectl get svc -n registry registry -o wide
```

说明：

- 记下 NodePort（示例：`31666`）
- 若 `registry` pod 拉镜像失败，先按基础文档预拉并重命名 `docker.io/library/registry:2`

---

## 3) 打包并上传 chart 到 registry

在本地仓库（含 litellm 源码）执行：

```bash
helm package /root/code/litellm/deploy/charts/litellm-helm --destination /root/code/k8s-at-home/build
scp /root/code/k8s-at-home/build/litellm-helm-1.1.0.tgz root@<ECS_IP>:/root/litellm-helm-1.1.0.tgz
```

在 ECS 主机执行（推荐本机 push，避免外网超时）：

```bash
helm push /root/litellm-helm-1.1.0.tgz oci://127.0.0.1:<REGISTRY_NODEPORT>/helm-charts --plain-http
```

---

## 4) 准备命名空间与 secrets

```bash
kubectl create namespace agents --dry-run=client -o yaml | kubectl apply -f -

kubectl create secret generic litellm-credentials -n agents \
  --from-literal=username='litellm' \
  --from-literal=password='NoTaGrEaTpAsSwOrD' \
  --from-literal=postgres-password='NoTaGrEaTpAsSwOrD' \
  --from-literal=redis-password='redis-pass-123' \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl create secret generic litellm-env-secret -n agents \
  --from-literal=KAOPU_API_KEY='REDACTED' \
  --dry-run=client -o yaml | kubectl apply -f -
```

关键点：

- `username` 必须存在（否则 LiteLLM Pod 会 `CreateContainerConfigError`）
- 本文档不要求真实上游 API key；本次验证重点是 DB secret 行为

---

## 5) 应用 ArgoCD Application

使用文件：`ecs/litellm/litellm.app.yaml`

```bash
kubectl apply -n argocd -f ecs/litellm/litellm.app.yaml
kubectl patch application litellm -n argocd --type merge -p '{"spec":{"syncPolicy":{"automated":{},"syncOptions":["CreateNamespace=true"]}}}'
kubectl annotate application litellm -n argocd argocd.argoproj.io/refresh=hard --overwrite
```

观察：

```bash
kubectl get application litellm -n argocd -w
kubectl get pods -n agents -w
```

---

## 6) 验证修复结果（必须执行）

### 6.1 Pod 状态

```bash
kubectl get pods -n agents -o wide
```

期望：

- `litellm-postgresql-0` `1/1 Running`
- `litellm-redis-master-0` `1/1 Running`
- `litellm-<hash>` `1/1 Running`

### 6.2 fallback secret 不应创建

```bash
kubectl get secret -n agents litellm-dbcredentials -o name
```

期望：

- 返回 `NotFound`（或无结果）

### 6.3 LiteLLM 日志无 DB 认证失败

```bash
kubectl logs -n agents deploy/litellm --tail=200
```

期望：

- 能看到启动完成/健康检查
- 不出现 `PrismaClientInitializationError` / `P1000`

---

## 7) 清理后重验（建议，证明改动稳健）

按顺序执行：

```bash
kubectl delete application litellm -n argocd --ignore-not-found
kubectl delete namespace agents --ignore-not-found

helm package /root/code/litellm/deploy/charts/litellm-helm --destination /root/code/k8s-at-home/build
scp /root/code/k8s-at-home/build/litellm-helm-1.1.0.tgz root@<ECS_IP>:/root/litellm-helm-1.1.0.tgz
ssh root@<ECS_IP> "helm push /root/litellm-helm-1.1.0.tgz oci://127.0.0.1:<REGISTRY_NODEPORT>/helm-charts --plain-http"

# 重新执行第 4~6 节
```

这一步通过，说明不是旧资源残留导致的“伪通过”。

---

## 8) Helm 模板检查（部署前快速自检）

在本地执行（示例）：

```bash
helm template litellm /root/code/litellm/deploy/charts/litellm-helm \
  --set db.deployStandalone=true \
  --set db.useExisting=false \
  --set db.dbCredentialsSecretName=litellm-credentials \
  --set postgresql.auth.existingSecret=litellm-credentials \
  --show-only templates/deployment.yaml

helm template litellm /root/code/litellm/deploy/charts/litellm-helm \
  --set db.deployStandalone=true \
  --set db.useExisting=false \
  --set db.dbCredentialsSecretName=litellm-credentials \
  --set postgresql.auth.existingSecret=litellm-credentials \
  --show-only templates/secret-dbcredentials.yaml
```

期望：

- `deployment.yaml` 里的 DB secret name 指向 `litellm-credentials`
- `secret-dbcredentials.yaml` 在该条件下不渲染

---

## 9) 常见问题与处理

### Q1: Pod 长时间 `ContainerCreating`

通常是镜像拉取慢，不一定是逻辑错误。

处理：

- 先 `kubectl describe pod ...` 看事件是否持续 `Pulling image`
- 用 `m.daocloud.io` 预拉并重命名目标镜像
- 删除对应 pod 重建

### Q2: PVC 一直 `Pending`

多见于 local-path helper busybox 拉取失败。

处理：

```bash
k3s ctr -n k8s.io images pull m.daocloud.io/docker.io/rancher/mirrored-library-busybox:1.37.0
k3s ctr -n k8s.io images tag m.daocloud.io/docker.io/rancher/mirrored-library-busybox:1.37.0 docker.io/rancher/mirrored-library-busybox:1.37.0
kubectl delete pod -n kube-system -l app=local-path-provisioner
```

### Q3: `CreateContainerConfigError` 且提示找不到 secret key

例如：`couldn't find key username in Secret agents/litellm-credentials`

处理：

```bash
kubectl patch secret -n agents litellm-credentials --type merge -p '{"stringData":{"username":"litellm"}}'
kubectl delete pod -n agents -l app.kubernetes.io/name=litellm
```

### Q4: ArgoCD 显示 `OutOfSync` 但服务已健康

可能由自动生成 secret 等对象导致漂移。对本验证目标（DB secret 不漂移）不是阻断项。

---

## 10) 隐私约束

- 不在文档、Issue、PR 中提交真实 `proxy_config.model_list` 业务配置。
- 共享复现时仅保留最小必要 YAML 与脱敏参数。
