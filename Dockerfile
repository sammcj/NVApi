# Build stage
FROM nvcr.io/nvidia/cuda:12.4.1-devel-ubuntu22.04 as builder

LABEL org.opencontainers.image.description "NVApi is a lightweight API that exposes NVIDIA GPU metrics"

# install go
RUN apt update && apt install -y golang git

WORKDIR /app

COPY . /app


# build the binary so it's portable and can run in a scratch container
RUN go build -o /app/nvapi main.go && \
  chmod +x /app/nvapi

# Runtime stage
#FROM nvcr.io/nvidia/cuda:12.4.1-runtime-ubuntu22.04

#WORKDIR /app

#COPY --from=builder /app/nvapi /app/nvapi

CMD ["/app/nvapi"]
