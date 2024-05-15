FROM golang:1.21.10
COPY server /server
WORKDIR /server
RUN go build -o fake-immudb .
ENTRYPOINT ["/server/fake-immudb"]
