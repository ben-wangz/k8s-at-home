ARG ALPINE_IMAGE=docker.io/library/alpine:latest
FROM ${ALPINE_IMAGE}

RUN apk add --no-cache aria2
COPY --chmod=755 entrypoint.sh /opt/aria2/bin/entrypoint.sh

CMD ["/opt/aria2/bin/entrypoint.sh"]
