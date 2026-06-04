FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY spike/tsnet-transport/main.go ./spike/tsnet-transport/main.go
RUN CGO_ENABLED=0 go build -o /spike-tsnet ./spike/tsnet-transport/

FROM ubuntu:24.04
RUN apt-get update && apt-get install -y --no-install-recommends \
        distcc build-essential ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /spike-tsnet /usr/local/bin/spike-tsnet
