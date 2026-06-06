FROM golang:1.25-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
ARG TARGET_CMD=node-service
COPY pkg ./pkg
COPY cmd/${TARGET_CMD} ./cmd/${TARGET_CMD}

ARG TARGET=./cmd/node-service
RUN CGO_ENABLED=0 go build -o /out/service ${TARGET}

FROM alpine:3.22
RUN apk add --no-cache e2fsprogs kmod nbd util-linux
COPY --from=build /out/service /service
ENTRYPOINT ["/service"]
