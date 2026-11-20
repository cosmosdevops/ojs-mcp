# ojs-mcp as a small image: one static binary and the CA certificates it
# needs to fetch schemas over HTTPS. Nothing else, not even a shell.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
# The build has no git history to read the version from, so it is passed in:
#   docker build --build-arg VERSION=1.2.3 .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.buildVersion=${VERSION}" -o /ojs-mcp .

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /ojs-mcp /ojs-mcp
# Runs as an unprivileged user. Files it must read (the API keys) need to
# be readable by this ID; see deploy/compose.yaml.
USER 65532:65532
EXPOSE 8087
ENTRYPOINT ["/ojs-mcp"]
# With no arguments it talks MCP over stdin/stdout, as the plain binary does:
#   docker run --rm -i ghcr.io/cosmosdevops/ojs-mcp
