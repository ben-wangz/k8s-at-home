ARG PODMAN_IMAGE=m.daocloud.io/quay.io/podman/stable:v5.6.1
FROM ${PODMAN_IMAGE}

LABEL maintainer="k8s-at-home"

ARG TINI_VERSION=v0.19.0
ADD --chmod=0755 https://github.com/krallin/tini/releases/download/${TINI_VERSION}/tini /tini

ENTRYPOINT ["/tini", "--"]

RUN dnf update -y \
    && dnf install -y openssh-server procps ncurses \
    && dnf clean all
RUN mkdir -p /var/run/sshd \
    && sed -i 's/#PermitRootLogin yes/PermitRootLogin without-password/' /etc/ssh/sshd_config \
    && sed -i 's/#PasswordAuthentication yes/PasswordAuthentication no/' /etc/ssh/sshd_config \
    && sed -ri 's/session required pam_loginuid.so/#session required pam_loginuid.so/g' /etc/pam.d/sshd

COPY --chmod=0755 entrypoint.sh /entrypoint.sh
CMD ["/entrypoint.sh"]
