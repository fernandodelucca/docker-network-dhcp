FROM golang:1.26.2-alpine AS builder

WORKDIR /usr/local/src/docker-net-dhcp
COPY go.* ./
RUN go mod download
RUN go mod verify

COPY cmd/ ./cmd/
COPY pkg/ ./pkg/
RUN mkdir bin/ && CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o bin/ ./cmd/...


FROM alpine:3.21

RUN mkdir -p /run/docker/plugins /var/lib/net-dhcp

COPY --from=builder /usr/local/src/docker-net-dhcp/bin/net-dhcp /usr/sbin/
COPY --from=builder /usr/local/src/docker-net-dhcp/bin/udhcpc-handler /usr/lib/net-dhcp/udhcpc-handler

ENTRYPOINT ["/usr/sbin/net-dhcp"]
