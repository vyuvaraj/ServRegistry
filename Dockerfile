FROM golang:1.24-alpine AS builder
WORKDIR /app
COPY . .
RUN sed -i 's/go 1\.2[5-9]\.[0-9]*/go 1.24/g' go.mod vendor/modules.txt && \
    CGO_ENABLED=0 go build -mod=vendor -o servregistry main.go

FROM alpine:latest
WORKDIR /app
COPY --from=builder /app/servregistry .
EXPOSE 8088
ENTRYPOINT ["./servregistry"]
