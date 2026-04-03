ARG NGINX_IMAGE=m.daocloud.io/docker.io/library/nginx:stable
FROM ${NGINX_IMAGE}
RUN apt-get update \
    && apt-get install -y moreutils netcat-openbsd curl unzip \
    && apt-get clean
ARG ARIA_NG_DOWNLOAD_URL=https://github.com/mayswind/AriaNg/releases/download/1.3.10/AriaNg-1.3.10.zip
RUN curl -L -o /tmp/AriaNg-1.3.10.zip $ARIA_NG_DOWNLOAD_URL \
    && unzip -o -d /usr/share/nginx/html/ /tmp/AriaNg-1.3.10.zip \
    && rm -f /tmp/AriaNg-1.3.10.zip
COPY nginx.conf /etc/nginx/nginx.conf
COPY --chmod=755 entrypoint.sh /opt/aria-ng/bin/entrypoint.sh

EXPOSE 8080

CMD ["/opt/aria-ng/bin/entrypoint.sh"]
