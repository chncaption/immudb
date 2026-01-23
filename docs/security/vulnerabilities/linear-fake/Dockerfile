FROM golang:1.26rc2
COPY server /server
WORKDIR /server
RUN go build -o fake-immudb .
ENTRYPOINT ["/server/fake-immudb"]
