# agentloop serve as a container, so a backend in any language can run
# thinking-core as a sidecar. See docs/cli/serve.md ("Running in a container").
#
#   docker build -t thinking-core .                  # distroless, the default
#   docker build --target alpine -t thinking-core .  # with a shell, for stdio MCP servers

# Keep in step with the go directive in go.mod; the CI job builds this file, so
# a drift shows up as a failed build rather than a surprise toolchain download.
FROM golang:1.25-alpine AS build
WORKDIR /src
# ext/mcp replaces the core with ../.., so the whole tree is the build context
# and the go.mod files are fetched first to keep the module layer cached.
COPY go.mod go.sum ./
COPY ext/mcp/go.mod ext/mcp/go.sum ./ext/mcp/
RUN cd ext/mcp && go mod download
COPY . .
ARG VERSION=dev
# The ext/mcp binary is the superset: the stock one cannot honour an "mcp"
# config section. CGO off because the SQLite driver is pure Go, which is what
# lets the runtime image be static with no libc.
RUN cd ext/mcp && CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/mind-vm/thinking-core/cmd/agentloop/cli.version=${VERSION}" \
      -o /out/agentloop ./cmd/agentloop \
 && mkdir /out/data

# Alpine variant: MCP stdio servers are separate programs (npx, python, ...)
# the image does not ship. Extend this stage, e.g.
#   FROM thinking-core:alpine
#   USER root
#   RUN apk add --no-cache nodejs npm
#   USER 65532
FROM alpine:3.22 AS alpine
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/agentloop /usr/local/bin/agentloop
# Same uid as distroless "nonroot", so a volume written by one image is
# readable by the other.
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
WORKDIR /data
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["agentloop", "serve"]
CMD ["--config", "/etc/thinking-core/config.json"]

# Default: no shell, no package manager, CA certificates included. It is the
# last stage so a plain `docker build` produces it.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/agentloop /usr/local/bin/agentloop
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
WORKDIR /data
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["agentloop", "serve"]
CMD ["--config", "/etc/thinking-core/config.json"]
