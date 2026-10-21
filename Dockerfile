FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/open-vault .

FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends restic ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 open-vault \
    && useradd --uid 10001 --gid open-vault --no-create-home --shell /usr/sbin/nologin open-vault
COPY --from=build /out/open-vault /usr/local/bin/open-vault
ENV XDG_CACHE_HOME=/tmp/open-vault-cache
USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/open-vault"]
CMD ["-config", "/etc/open-vault/config.json"]
