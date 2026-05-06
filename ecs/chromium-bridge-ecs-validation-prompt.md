# Chromium Bridge ECS Validation Prompt

Use this prompt in a fresh OpenCode session. Replace `<ECS_HOST>` before running.

```text
你现在要在远端 ECS `<ECS_HOST>` 上，完成 `chromium-bridge` 的部署可用性验证。请严格遵循以下要求。

【目标】
1) 仅在远端 ECS 执行，不做本地替代验证。
2) 使用已发布产物进行安装：
   - Helm chart: `oci://ghcr.io/ben-wangz/k8s-at-home-charts/chromium-bridge`
   - images:
     - `ghcr.io/ben-wangz/k8s-at-home-chromium-bridge-server:<tag>`
     - `ghcr.io/ben-wangz/k8s-at-home-chromium-bridge-novnc:<tag>`
3) 验证 CDP 与 noVNC 可用，并执行一次 Playwright `connectOverCDP` 截图验证。

【已知约束】
- chart 默认 values 在仓库：`application/chromium-bridge/chart/values.yaml`
- 模板目录结构：
  - `application/chromium-bridge/chart/templates/server/*`
  - `application/chromium-bridge/chart/templates/novnc/*`
- 不再使用：
  - `application/chromium-bridge/chart/values-ecs.yaml`
  - 本地构建+推送 registry 的旧流程

【执行步骤（必须按顺序）】

Step 0. SSH 与基础检查
- 连接 `root@<ECS_HOST>`。
- 检查 `kubectl`、`helm`、`node`、`npm` 是否可用。
- `kubectl get nodes -o wide`，确认集群正常。

Step 1. 获取 chart 版本并安装
- 在仓库根目录执行：
  - `FORGEKIT_BIN="$(bash setup/forgekit.sh)"`
  - `CHART_VERSION="$($FORGEKIT_BIN version get chart chromium-bridge)"`
- 安装命令（允许通过 `--set` 覆盖 NodePort/ingress）：
  - `helm upgrade --install chromium-bridge oci://ghcr.io/ben-wangz/k8s-at-home-charts/chromium-bridge --version ${CHART_VERSION} --namespace chromium-bridge --create-namespace --atomic`

Step 2. 覆盖 ECS 访问参数（如需要）
- 按环境设置 `server.service.type/novnc.service.type`（通常 NodePort）。
- 记录实际端口：
  - CDP: server service 的 `9222`
  - noVNC: novnc service 的 `6080`

Step 3. 资源健康检查
- `kubectl get pods -n chromium-bridge -o wide`
- `kubectl get svc -n chromium-bridge -o wide`
- 若异常，附带：
  - `kubectl describe pod -n chromium-bridge <pod>`
  - `kubectl logs -n chromium-bridge <pod> --all-containers --tail=200`

Step 4. 功能验证（必须）
- CDP:
  - `curl -sS http://127.0.0.1:<CDP_NODEPORT>/json/version`
  - 必须出现 `webSocketDebuggerUrl`
- noVNC:
  - `curl -I http://127.0.0.1:<NOVNC_NODEPORT>/`
  - 必须返回 `HTTP/1.1 200`

Step 5. Playwright 验证（必须）
- 若缺失则安装：`npm i playwright`。
- 写一个最小脚本用 `connectOverCDP("http://127.0.0.1:<CDP_NODEPORT>")`。
- 访问 `https://example.com`，输出 title 并截图到：
  - `/root/chromium-bridge-e2e/cdp-e2e.png`

【最终输出格式要求】
最后只输出以下内容（不要长篇解释）：
1) 执行结果总览（成功/失败）
2) 关键资源状态（nodes/pods/services）
3) CDP/noVNC/Playwright 验证结果
4) 关键产物路径（截图、脚本）
5) 若失败，给出下一步最小修复动作（最多 3 条）
```
