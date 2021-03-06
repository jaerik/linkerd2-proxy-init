## compile proxy-init utility
FROM golang:1.12.9 as golang
WORKDIR /build
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/linkerd2-proxy-init -mod=readonly -ldflags "-s -w" -v

## package runtime
FROM debian:stretch-20190812-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        iptables \
        procps \
    && rm -rf /var/lib/apt/lists/*
COPY LICENSE /linkerd/LICENSE
COPY --from=golang /out/linkerd2-proxy-init /usr/local/bin/proxy-init
ENTRYPOINT ["/usr/local/bin/proxy-init"]
