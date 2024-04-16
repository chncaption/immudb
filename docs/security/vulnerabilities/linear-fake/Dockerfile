FROM golang:1.22.2
COPY server /server
WORKDIR /server
RUN go build -o fake-immudb .
ENTRYPOINT ["/server/fake-immudb"]
