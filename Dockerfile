FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/relay .

# Pre-create the cache dir with the nonroot UID/GID (65532 in distroless).
# When Docker mounts a fresh named volume here on first start, it inherits
# this ownership -- otherwise the volume is root:root and the nonroot relay
# can't write to it.
RUN mkdir -p /out/cache && chown -R 65532:65532 /out/cache

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/relay /relay
COPY --from=build --chown=65532:65532 /out/cache /var/cache/relay
USER nonroot:nonroot
EXPOSE 8080
VOLUME ["/var/cache/relay"]
ENTRYPOINT ["/relay"]
