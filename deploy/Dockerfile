# Build stage
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO is not required: omni-notify uses the pure-Go modernc.org/sqlite driver.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/omni-notify ./cmd/omni-notify

# Runtime stage
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/omni-notify /app/omni-notify
# Provide config.yaml and a writable volume for the database at runtime.
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["/app/omni-notify"]
CMD ["-config", "/app/config.yaml"]
