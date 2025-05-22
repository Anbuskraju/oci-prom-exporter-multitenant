FROM golang:1.21-alpine

WORKDIR /app

COPY . .

RUN go build -o oci_exporter main.go

EXPOSE 2112

CMD ["./oci_exporter"]
