FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/relay .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/relay /relay
USER nonroot:nonroot
EXPOSE 8080
VOLUME ["/var/cache/relay"]
ENTRYPOINT ["/relay"]
