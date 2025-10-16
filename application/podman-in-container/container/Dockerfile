ARG PODMAN_IMAGE=m.daocloud.io/quay.io/podman/stable:v5.6.1
FROM ${PODMAN_IMAGE}

LABEL maintainer="k8s-at-home"

# Setup environment variables
ENV PNPM_HOME="/opt/pnpm" \
    PATH="$PATH:/opt/pnpm" \
    LANG=en_US.UTF-8 \
    LC_ALL=en_US.UTF-8

# Install system packages, openssh-server, nodejs, locale support, claude-code and iflow-cli
RUN dnf update -y \
    && dnf install -y procps ncurses curl which screen tree \
       glibc-locale-source glibc-langpack-zh \
       openssh-server \
    && curl -fsSL https://rpm.nodesource.com/setup_lts.x | bash - \
    && dnf install -y nodejs \
    && localedef -c -f UTF-8 -i en_US en_US.UTF-8 \
    && dnf clean all \
    && npm install -g pnpm \
    && mkdir -p ${PNPM_HOME} \
    && pnpm config set global-bin-dir ${PNPM_HOME} \
    && pnpm add -g @anthropic-ai/claude-code \
    && pnpm add -g @iflow-ai/iflow-cli

# Configure SSH
RUN mkdir -p /var/run/sshd \
    && sed -i 's/#PermitRootLogin yes/PermitRootLogin without-password/' /etc/ssh/sshd_config \
    && sed -i 's/#PasswordAuthentication yes/PasswordAuthentication no/' /etc/ssh/sshd_config \
    && sed -ri 's/session required pam_loginuid.so/#session required pam_loginuid.so/g' /etc/pam.d/sshd

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
