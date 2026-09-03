FROM golang:1.26-alpine AS builder

# The final image is scratch, which carries no trust store. Without one,
# every HTTPS call to AWX fails with "x509: certificate signed by unknown
# authority" - including instances using an ordinary public CA.
RUN apk add --no-cache ca-certificates

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -o /app/main .

FROM scratch

WORKDIR /app

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /app/main .

# Numeric so the kubelet can enforce runAsNonRoot without a passwd entry.
USER 65532:65532

ENTRYPOINT ["./main"]
