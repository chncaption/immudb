FROM golang:1.20.11
COPY server /server
WORKDIR /server
RUN go build -o fake-immudb .
ENTRYPOINT ["/server/fake-immudb"]
