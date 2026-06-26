FROM golang:1.24.1-alpine AS builder

ENV CGO_ENABLED=0

WORKDIR /workspace

ADD go.mod go.sum ./
RUN go mod download
ADD . .
RUN go build -o .build/deployer -ldflags "-w -s" .

FROM gcr.io/distroless/static

WORKDIR /app

COPY --from=builder --link /workspace/.build/* ./
ENTRYPOINT ["/app/deployer"]
