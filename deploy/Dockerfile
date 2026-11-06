# Named source contexts are pinned by the backend and frontend Foundation locks.
# No unpublished @platform npm package or Go module is downloaded from a registry.
FROM node:24-alpine AS admin
ARG NPM_CONFIG_REGISTRY=https://registry.npmjs.org
WORKDIR /src/av-platform/livekit-app
COPY --from=foundation . /src/platform-foundation
COPY --from=app . .
COPY api/openapi.json /src/av-platform/livekit-server/api/openapi.json
RUN --mount=type=cache,target=/root/.npm node scripts/build-admin.mjs --foundation-root /src/platform-foundation

FROM golang:1.25-alpine AS build
ARG GOPROXY=https://proxy.golang.org,direct
ARG GOSUMDB=sum.golang.org
WORKDIR /src/av-platform/livekit-server
ENV CGO_ENABLED=0 GOWORK=off
COPY --from=foundation platform-foundation-server /src/platform-foundation/platform-foundation-server
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath -o /out/business-api ./cmd/business-api

FROM alpine:3.22
RUN apk add --no-cache ca-certificates && addgroup -g 10001 av && adduser -D -u 10001 -G av av
COPY --from=build /out/business-api /usr/local/bin/business-api
COPY --from=admin /src/av-platform/livekit-app/apps/admin/web/dist /opt/av/admin
ENV AV_ADMIN_WEB=/opt/av/admin
USER av
EXPOSE 8080
ENTRYPOINT ["business-api"]
CMD ["serve"]
