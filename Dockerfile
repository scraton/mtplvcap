FROM golang:1.19-alpine AS builder

RUN apk --no-cache add -t build-deps build-base pkgconfig make git curl libusb-dev

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_CFLAGS='-Wno-deprecated-declarations' go build .

FROM alpine:3.15

WORKDIR /app
ENV PATH=/app:$PATH

RUN apk --no-cache add libusb
COPY --from=builder /src/mtplvcap /usr/bin/mtplvcap

EXPOSE 42839

CMD ["mtplvcap", "-host", "0.0.0.0"]
