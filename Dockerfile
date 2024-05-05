FROM golang:alpine

WORKDIR /app

COPY . /app

RUN go build -o main main.go

EXPOSE 9999

CMD ["main", "-port", "9999", "-rate", "1"]
