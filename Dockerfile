FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -mod=mod -o /out/fias-downloader ./cmd/fias-downloader

FROM alpine:3.20
RUN apk add --no-cache su-exec
RUN adduser -D -u 10001 app
WORKDIR /app
COPY --from=build /out/fias-downloader /app/fias-downloader
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh /app/fias-downloader
VOLUME ["/app/data"]
EXPOSE 8080
ENTRYPOINT ["/entrypoint.sh"]
CMD ["/app/fias-downloader"]
