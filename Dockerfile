# nginx with Brutal congestion control for QUIC / HTTP3.
#
# Two-stage: compile against the same Debian release that the runtime image
# uses, otherwise the binary's glibc/OpenSSL symbols will not resolve.

ARG NGINX_VERSION=1.31.8

# Pinned to the exact release the binary is compiled from.  Floating on
# nginx:mainline would eventually pair a 1.31.8 binary with a newer image
# whose bundled dynamic modules are signature-incompatible: load_module
# then refuses to load them at startup.
ARG BASE=nginx:1.31.8

# sha256 of https://nginx.org/download/nginx-${NGINX_VERSION}.tar.gz
ARG NGINX_SHA256=0652b760b3b772d2ec17b47c2fc15d9a95e6fe35cd5b877eadc1a62d9bc75af1

# No --platform=$BUILDPLATFORM here on purpose. Pinning it would run this
# stage on the build host's native arch and silently produce an x86 binary
# inside the arm64 image. Letting qemu emulate is slower but correct.
FROM debian:trixie AS builder

ARG NGINX_VERSION
ARG NGINX_SHA256

RUN apt-get update && apt-get install -y --no-install-recommends \
        build-essential ca-certificates curl \
        libpcre2-dev zlib1g-dev libssl-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src

# Verify before unpacking: the tarball is fetched over the network and
# becomes the entire server binary.
RUN set -eu; \
    curl -fsSL -o /src/nginx.tar.gz \
        "https://nginx.org/download/nginx-${NGINX_VERSION}.tar.gz"; \
    echo "${NGINX_SHA256}  /src/nginx.tar.gz" | sha256sum -c -; \
    tar xzf /src/nginx.tar.gz --strip-components=1; \
    rm -f /src/nginx.tar.gz

COPY patches/nginx-brutal-quic.patch /src/
RUN patch -p1 < /src/nginx-brutal-quic.patch

# Mirrors the flags of the official nginx:mainline image so this binary is a
# drop-in replacement (same module set, same paths, same module ABI).
RUN ./configure \
        --prefix=/etc/nginx \
        --sbin-path=/usr/sbin/nginx \
        --modules-path=/usr/lib/nginx/modules \
        --conf-path=/etc/nginx/nginx.conf \
        --error-log-path=/var/log/nginx/error.log \
        --http-log-path=/var/log/nginx/access.log \
        --http-client-body-temp-path=/var/cache/nginx/client_temp \
        --http-proxy-temp-path=/var/cache/nginx/proxy_temp \
        --http-fastcgi-temp-path=/var/cache/nginx/fastcgi_temp \
        --http-uwsgi-temp-path=/var/cache/nginx/uwsgi_temp \
        --http-scgi-temp-path=/var/cache/nginx/scgi_temp \
        --pid-path=/run/nginx.pid \
        --lock-path=/run/nginx.lock \
        --user=nginx \
        --group=nginx \
        --with-compat \
        --with-control-api \
        --with-file-aio \
        --with-threads \
        --with-http_addition_module \
        --with-http_auth_request_module \
        --with-http_dav_module \
        --with-http_flv_module \
        --with-http_gunzip_module \
        --with-http_gzip_static_module \
        --with-http_json_module \
        --with-http_mp4_module \
        --with-http_random_index_module \
        --with-http_realip_module \
        --with-http_secure_link_module \
        --with-http_slice_module \
        --with-http_ssl_module \
        --with-http_stub_status_module \
        --with-http_sub_module \
        --with-http_v2_module \
        --with-http_v3_module \
        --with-mail \
        --with-mail_ssl_module \
        --with-stream \
        --with-stream_realip_module \
        --with-stream_ssl_module \
        --with-stream_ssl_preread_module \
        --with-cc-opt="-O2 -Werror -U_FORTIFY_SOURCE \
                       -Wp,-D_FORTIFY_SOURCE=2 -fstack-protector-strong -fPIC" \
        --with-ld-opt="-Wl,-z,relro -Wl,-z,now" \
    && make -j"$(nproc)"

# Only the binary is taken from the build: the official image already ships
# mime.types, the default conf tree, the nginx user and the module dir.
# Note --sbin-path only takes effect on `make install`; a plain `make`
# leaves the binary in objs/.


FROM ${BASE}

ARG NGINX_VERSION
LABEL org.opencontainers.image.title="nginx-brutal-quic" \
      org.opencontainers.image.description="nginx ${NGINX_VERSION} with hysteria2-style Brutal congestion control for QUIC" \
      org.opencontainers.image.source="https://github.com/iliyian/nginx-brutal-quic" \
      org.opencontainers.image.licenses="BSD-2-Clause"

COPY --from=builder /src/objs/nginx /usr/sbin/nginx

# Sanity check: the binary must be for this image's architecture and must
# carry the HTTP/3 module the patch hangs off of.
RUN set -eu; \
    nginx -V 2>&1 | grep -q with-http_v3_module; \
    nginx -v 2>&1; \
    echo "arch=$(uname -m)"
