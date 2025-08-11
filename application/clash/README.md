# clash

## introduction

Clash is a powerful proxy tool that supports multiple protocols and provides flexible routing rules. It can help you access the internet securely and efficiently.

## resources

1. helm chart
    * [clash](chart/)

2. install with helm
    * prepare `clash/config.yaml` for clash
    * create secret named `clash-config` with directory `clash`
        + ```shell
          kubectl -n basic-components create secret generic clash-config --from-file=clash/
          ```
    * ```shell
      helm upgrade --install clash oci://ghcr.io/ben-wangz/k8s-at-home-charts/clash \
        --atomic \
        --version 1.0.0 \
        --namespace basic-components \
        --create-namespace \
        --set clash.image.repository=m.daocloud.io/docker.io/dreamacro/clash \
        --set config.existingSecret=clash-config \
        --set clash.timezone="Asia/Shanghai"
      ```
