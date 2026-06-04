FROM golang:1.26 AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /distcc-action ./cmd/distcc-action

FROM ubuntu:24.04
RUN apt-get update && apt-get install -y --no-install-recommends \
        distcc build-essential git ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /distcc-action /usr/local/bin/distcc-action
