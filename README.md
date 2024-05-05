# NVApi

A lightweight API that returns Nvidia GPU utilisation information.

## Usage

To run the API, use the following command:

```
go run main.go -port 9999 -rate 1
```

This will start the API on port 9999 with a rate limit of 1 request per second.

To build a Docker image, use the following command:

```
docker build -t my-nvapi .
```
To run the Docker container, use the following command:

```
docker run -p 9999:9999 my-nvapi
```

## API Endpoints


### `/gpu`

Returns the current GPU utilisation information in JSON format.

## Query Parameters


* `port`: The port number to listen on (default: 9999)
* `rate`: The minimum number of seconds between requests (default: 1)

## Example Response

```json
{
  "gpu_utilization": 50,
  "memory_utilization": 30,
  "power_watts": 200,
  "memory_total_gb": 12.0,
  "memory_used_gb": 4.0,
  "memory_free_gb": 8.0,
  "memory_usage": "33%",
  "temperature": 50,
  "fan_speed": 30
}
```
## License


This project is licensed under the MIT License. See [LICENSE](LICENSE) for details.
