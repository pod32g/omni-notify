# syntax=docker/dockerfile:1

# --- build stage: compile a static, CGO-free binary (pure-Go SQLite driver, so
#     the runtime image needs nothing else) ---
FROM golang:1.26-bookworm AS build
WORKDIR /src

# Cache module downloads separately from the source for faster rebuilds.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=0.1.0
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /omni-notify ./cmd/omni-notify

# --- runtime stage: distroless static. The `omni-notify healthcheck` subcommand
#     lets the container self-probe without a shell or curl. ---
FROM gcr.io/distroless/static-debian12

COPY --from=build /omni-notify /omni-notify

EXPOSE 8080
VOLUME ["/data"]

ENTRYPOINT ["/omni-notify"]
CMD ["-config", "/etc/omni-notify/config.yml"]
