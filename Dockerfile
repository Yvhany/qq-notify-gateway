# ---- build stage ----
FROM golang:1.27-alpine AS build
WORKDIR /src
ENV GOPROXY=https://goproxy.cn,direct
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/qq-notify-gateway .

# ---- runtime stage ----
FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 10001 app
USER app
COPY --from=build /out/qq-notify-gateway /usr/local/bin/qq-notify-gateway
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/qq-notify-gateway"]
