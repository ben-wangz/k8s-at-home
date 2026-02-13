ARG PODMAN_IMAGE=m.daocloud.io/quay.io/podman/stable:v5.6.1
FROM ${PODMAN_IMAGE}

LABEL maintainer="k8s-at-home"

# Disable netns="host" to use default bridge mode
RUN sed -i 's/^netns="host"$/# netns="host"  # Commented out to use default bridge mode for proper port mapping/' /etc/containers/containers.conf

# Setup environment variables
ENV PATH="$PATH:/opt/nodejs/bin" \
    LANG=en_US.UTF-8 \
    LC_ALL=en_US.UTF-8

# Install system packages, openssh-server, nodejs and locale support
ARG NODEJS_VERSION=v24.13.1
RUN dnf update -y \
    && dnf install -y procps ncurses curl which screen tree unzip openssl jq yq \
       glibc-locale-source glibc-langpack-zh \
       openssh-server vim \
    && localedef -c -f UTF-8 -i en_US en_US.UTF-8 \
    && curl -fsSL https://nodejs.org/dist/${NODEJS_VERSION}/node-${NODEJS_VERSION}-linux-x64.tar.xz -o /tmp/nodejs.tar.xz \
    && mkdir -p /opt/nodejs \
    && tar -xJf /tmp/nodejs.tar.xz -C /opt/nodejs --strip-components=1 \
    && rm /tmp/nodejs.tar.xz \
    && dnf clean all

# Configure SSH and setup login message
RUN mkdir -p /var/run/sshd \
    && sed -i 's/#PermitRootLogin yes/PermitRootLogin without-password/' /etc/ssh/sshd_config \
    && sed -i 's/#PasswordAuthentication yes/PasswordAuthentication no/' /etc/ssh/sshd_config \
    && sed -ri 's/session required pam_loginuid.so/#session required pam_loginuid.so/g' /etc/pam.d/sshd \
    && echo "Common tools guide: /opt/codespace/docs/common-tools.md" > /etc/motd

# Copy documentation
RUN mkdir -p /opt/codespace/docs
COPY --chmod=0644 common-tools.md /opt/codespace/docs/

# Copy entrypoint script
COPY --chmod=0755 entrypoint.sh /entrypoint.sh

# Install openvscode-server
ARG OPENVSCODE_VERSION=v1.103.1
RUN curl -LO https://github.com/gitpod-io/openvscode-server/releases/download/openvscode-server-${OPENVSCODE_VERSION}/openvscode-server-${OPENVSCODE_VERSION}-linux-x64.tar.gz \
    && mkdir -p /opt/openvscode-server \
    && tar zxvf openvscode-server-${OPENVSCODE_VERSION}-linux-x64.tar.gz -C /opt/openvscode-server \
    && ln -s /opt/openvscode-server/openvscode-server-${OPENVSCODE_VERSION}-linux-x64 /opt/openvscode-server/current \
    && rm openvscode-server-${OPENVSCODE_VERSION}-linux-x64.tar.gz

# Download tini (moved down to improve cache utilization)
ARG TINI_VERSION=v0.19.0
ADD --chmod=0755 https://github.com/krallin/tini/releases/download/${TINI_VERSION}/tini /tini

ENTRYPOINT ["/tini", "--"]
CMD ["/entrypoint.sh"]
