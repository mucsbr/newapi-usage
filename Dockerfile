FROM golang:1.25-alpine AS build

ARG APK_MIRROR=https://mirrors.aliyun.com/alpine
ARG GOPROXY=https://goproxy.cn,direct
ARG GOSUMDB=sum.golang.google.cn

ENV GOPROXY=${GOPROXY} \
  GOSUMDB=${GOSUMDB}

WORKDIR /src
RUN if [ -n "$APK_MIRROR" ]; then \
    sed -i "s|https://dl-cdn.alpinelinux.org/alpine|${APK_MIRROR}|g" /etc/apk/repositories; \
  fi \
  && apk add --no-cache ca-certificates git

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/newapi-usage ./cmd/newapi-usage

FROM alpine:3.22

ARG APK_MIRROR=https://mirrors.aliyun.com/alpine

RUN if [ -n "$APK_MIRROR" ]; then \
    sed -i "s|https://dl-cdn.alpinelinux.org/alpine|${APK_MIRROR}|g" /etc/apk/repositories; \
  fi \
  && apk add --no-cache ca-certificates \
  && adduser -D -H -u 10001 app

COPY --from=build /out/newapi-usage /usr/local/bin/newapi-usage

USER app
EXPOSE 8080
ENTRYPOINT ["newapi-usage"]
