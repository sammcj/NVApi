# NVApi

A lightweight API that returns Nvidia GPU utilisation information.

![](screenshots/json_response.png)

## Usage

### Docker Container

_Note: The Dockerfile is a work in progress, the current container image is bloated and not optimised for size yet._

The application can be run as a container:

```shell
docker build -t nvapi:latest .
```

Or using docker-compose see [docker-compose.yml](docker-compose.yml) for an example configuration.

### Local Installation

To run the API, use the following command:

```
go run main.go -port 9999 -rate 1
```

This will start the API on port 9999 with a rate limit of 1 request per second.

## API Endpoints


### `/`

Returns the current GPU utilisation information in JSON format.

## Query Parameters


* `port`: The port number to listen on (default: 9999)
* `rate`: The minimum number of seconds between requests (default: 3)

## Example Response

```shell
curl http://localhost:9999/gpu
```

```json
[{
	"gpu_utilisation": 0,
	"memory_utilisation": 0,
	"power_watts": 23,
	"memory_total_gb": 24,
	"memory_used_gb": 9.77,
	"memory_free_gb": 14.23,
	"memory_usage_percent": 41,
	"temperature": 35,
	"fan_speed": 0,
	"processes": [{
		"Pid": 2345831,
		"UsedGpuMemoryMb": 9678,
		"Name": "/tmp/ollama630272566/runners/cuda_v12/ollama_llama_server",
		"Arguments": ["--model", "/home/llm/.ollama/models/blobs/sha256-583c616da14b82930f887f991ab446711da0b029166200b67892d7c9f8f45958", "--ctx-size", "12288", "--batch-size", "512", "--embedding", "--log-disable", "--n-gpu-layers", "33", "--flash-attn", "--parallel", "6", "--port", "42161"]
	}]
}]
```

### `/svg`

Returns a (very) basic SVG image showing the current GPU utilisation as a bar.

### Home Assistant Integration

![](screenshots/home-assistant-integration-2.png)

Example of using the API to integrate with Home Assistant:

```yaml
sensors:

- platform: rest
  name: "GPU Utilisation"
  resource: http://localhost:9999
  unit_of_measurement: "%"
  unique_id: gpu_0
  scan_interval: 30
  json_attributes:
    - gpu_utilisation
    - memory_utilisation
    - memory_used_gb
    - memory_free_gb
    - power_watts
    - temperature
    - fan_speed
  value_template: '{{ value_json[0].gpu_utilisation }}'

- platform: rest
  name: "GPU Memory Utilisation"
  resource: http://localhost:9999
  unit_of_measurement: "%"
  unique_id: gpu_0_memory_utilisation
  scan_interval: 30
  json_attributes:
    - memory_utilisation
  value_template: '{{ value_json[0].memory_utilisation }}'

- platform: rest
  name: "GPU Memory Used"
  resource: http://localhost:9999
  unit_of_measurement: "GB"
  unique_id: gpu_0_memory_used_gb
  scan_interval: 30
  json_attributes:
    - memory_used_gb
  value_template: '{{ value_json[0].memory_used_gb }}'

- platform: rest
  name: "GPU Memory Free"
  resource: http://localhost:9999
  unit_of_measurement: "GB"
  unique_id: gpu_0_memory_free_gb
  scan_interval: 30
  json_attributes:
    - memory_free_gb
  value_template: '{{ value_json[0].memory_free_gb }}'

- platform: rest
  name: "GPU Temperature"
  resource: http://localhost:9999
  unit_of_measurement: "°C"
  unique_id: gpu_0_temperature
  scan_interval: 30
  json_attributes:
    - temperature
  value_template: '{{ value_json[0].temperature }}'

- platform: rest
  name: "GPU Fan Speed"
  resource: http://localhost:9999
  unit_of_measurement: "RPM"
  unique_id: gpu_0_fan_speed
  scan_interval: 30
  json_attributes:
    - fan_speed
  value_template: '{{ value_json[0].fan_speed }}'

- platform: rest
  name: "GPU Power"
  resource: http://localhost:9999
  unit_of_measurement: "W"
  unique_id: gpu_0_power
  scan_interval: 30
  json_attributes:
    - power_watts
  value_template: '{{ value_json[0].power_watts }}'
```

## NVApi-Tray GUI

A simple GUI application that displays the GPU utilisation information from the API.

![](screenshots/NVApiGUI.png)

This is a work in progress but can be built from the `NVApi-GUI` directory.

```shell
cd NVApi-GUI
go build
```

## License

Copyright 2024 Sam McLeod

This project is licensed under the MIT License. See [LICENSE](LICENSE) for details.
