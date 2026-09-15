# Single Dockerfile for both services; the target binary is chosen with the
# SERVICE build argument so the dependency layer is shared between them.
FROM golang:1.25-alpine AS build

ARG SERVICE=api
WORKDIR /src

# Dependencies are copied first so a code change does not invalidate the
# module download layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath -ldflags="-s -w" \
    -o /out/service ./cmd/${SERVICE}

FROM alpine:3.20

# The engine talks to the Docker daemon over a socket and to Postgres/Redis
# over TLS-capable connections; ca-certificates covers outbound webhooks.
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 10001 ocee

COPY --from=build /out/service /usr/local/bin/service

USER ocee
ENTRYPOINT ["/usr/local/bin/service"]
