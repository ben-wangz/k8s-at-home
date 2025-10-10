# aria2

## introduction

aria2 is a download tool, which supports multi connections and multi protocols.

## resources

1. container
    * [aria2](container/aria2/)
    * [aria-ng](container/aria-ng/)
2. helm chart
    * [aria2](chart/)
3. install with helm
    * ```shell
      # Get the latest chart version
      export CHART_VERSION=$(bash ../../tools/get-version.sh aria2 chart)

      helm upgrade --install aria2 oci://ghcr.io/ben-wangz/k8s-at-home-charts/aria2 \
        --atomic \
        --version ${CHART_VERSION} \
        --namespace default \
        --create-namespace \
        --set aria2.rpcSecret="your-rpc-secret" \
        --set aria2.timezone="Asia/Shanghai" \
        --set service.type=ClusterIP
      ```
