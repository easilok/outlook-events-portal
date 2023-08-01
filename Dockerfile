# syntax=docker/dockerfile:1

FROM golang:1.19

# Set destination for COPY
WORKDIR /app

COPY . .

# ENV GIN_MODE=release
RUN go get -d -v ./...
RUN go install -v ./...

EXPOSE 8080

RUN mkdir /cache
VOLUME /cache

CMD ["outlook_event_reading"]
