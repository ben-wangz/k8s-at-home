# clash

## introduction

Clash is a powerful proxy tool that supports multiple protocols and provides flexible routing rules. It can help you access the internet securely and efficiently.

## resources

1. helm chart
    * [clash](chart/)

2. install with helm
    * ```shell
      helm upgrade --install clash oci://ghcr.io/ben-wangz/k8s-at-home/clash \
        --atomic \
        --version 1.0.0 \
        --namespace default \
        --create-namespace \
        --set replicas=1 \
        --set clash.timezone="Asia/Shanghai" \
        --set service.type=ClusterIP
      ```
