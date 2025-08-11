# clash

## introduction

Clash is a powerful proxy tool that supports multiple protocols and provides flexible routing rules. It can help you access the internet securely and efficiently.

## resources

1. helm chart
    * [clash](chart/)

2. install with helm
    * prepare `clash/config.yaml` for clash
        + start subconverter
            * ```shell
              podman run --name subconverter --rm -d docker.io/tindy2013/subconverter:latest
              ```
        + encode subscription
            * ```shell
              SUBSCRIPTION_URL="https://example.com/subscription"
              podman run --rm -it docker.io/library/python:alpine python -c "import urllib.parse; print(urllib.parse.quote('$SUBSCRIPTION_URL', safe=''))"
              ```
        + generate `clash/config.yaml`
            * ```shell
              ENCODED_SUBSCRIPTION_URL=your-encoded-subscription-url
              podman exec -it subconverter sh -c \
                  "wget -O /tmp/config.yaml 'http://localhost:25500/sub?url=$ENCODED_SUBSCRIPTION_URL&target=clash'"
              podman cp subconverter:/tmp/config.yaml clash/config.yaml
              ```
        + replace `external-controller`
            * ```shell
              sed -i 's/external-controller: 127.0.0.1:9090/external-controller: 0.0.0.0:9090/g' clash/config.yaml
              ```
    * create secret named `clash-config` with directory `clash`
        + ```shell
          kubectl -n basic-components create secret generic clash-config --from-file=clash/
          ```
    * ```shell
      helm upgrade --install clash oci://ghcr.io/ben-wangz/k8s-at-home-charts/clash \
        --atomic \
        --version 1.1.0 \
        --namespace basic-components \
        --create-namespace \
        --set clash.image.repository=m.daocloud.io/docker.io/dreamacro/clash \
        --set config.existingSecret=clash-config \
        --set clash.timezone="Asia/Shanghai" \
        --set service.type=NodePort \
        --set service.ports.httpNodePort=32789 \
        --set service.ports.controllerNodePort=32909
      ```
