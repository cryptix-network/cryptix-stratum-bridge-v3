FROM golang:1.19.1 as builder

LABEL org.opencontainers.image.description="Dockerized Cryptix Stratum Bridge"
LABEL org.opencontainers.image.authors="cryptix"
LABEL org.opencontainers.image.source="https://github.com/cryptix-network/cryptix-stratum-bridge-v3"

WORKDIR /go/src/app
ADD go.mod .
ADD go.sum .
RUN go mod download

ADD . .
RUN go build -o /go/bin/app ./cmd/cryptixbridge


FROM gcr.io/distroless/base:nonroot
COPY --from=builder /go/bin/app /
COPY cmd/cryptixbridge/config.yaml /

WORKDIR /
ENTRYPOINT ["/app"]
