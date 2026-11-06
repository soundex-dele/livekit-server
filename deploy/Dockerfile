# The named foundation context is supplied by compose; no published module is assumed.
FROM golang:1.25-alpine AS build
ARG GOPROXY=https://proxy.golang.org,direct
ARG GOSUMDB=sum.golang.org
WORKDIR /src/av-platform/livekit-server
ENV CGO_ENABLED=0 GOWORK=off
COPY --from=foundation . /src/platform-foundation/platform-foundation-server
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath -o /out/business-api ./cmd/business-api

FROM alpine:3.22
RUN apk add --no-cache ca-certificates && addgroup -g 10001 av && adduser -D -u 10001 -G av av
COPY --from=build /out/business-api /usr/local/bin/business-api
USER av
EXPOSE 8080
ENTRYPOINT ["business-api"]
CMD ["serve"]
