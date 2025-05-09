FROM golang:1.24.3
COPY server /server
WORKDIR /server
RUN go build -o fake-immudb .
ENTRYPOINT ["/server/fake-immudb"]
