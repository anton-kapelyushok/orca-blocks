FROM golang:1.25-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

ARG TARGET=./cmd/node-service
RUN CGO_ENABLED=0 go build -o /out/service ${TARGET}

FROM alpine:3.22
COPY --from=build /out/service /service
ENTRYPOINT ["/service"]
