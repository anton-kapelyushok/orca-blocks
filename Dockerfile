FROM golang:1.25-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
ARG TARGET_CMD=node-service
COPY pkg ./pkg
COPY cmd ./cmd

ARG TARGET=./cmd/node-service
RUN CGO_ENABLED=0 go build -o /out/service ${TARGET}
RUN BUILD_TIME_UTC="$(date -u +%Y-%m-%dT%H:%M:%SZ)" && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.buildTimeUTC=${BUILD_TIME_UTC}" -o /out/orca-init ./cmd/orca-init

FROM alpine:3.22
RUN apk add --no-cache docker-cli e2fsprogs iproute2 iptables kmod nbd tar util-linux
COPY --from=build /out/service /service
COPY --from=build /out/orca-init /orca-init
ENTRYPOINT ["/service"]
