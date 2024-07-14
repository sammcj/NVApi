# NVApi

A lightweight API that returns Nvidia GPU utilisation information.

![](screenshots/json_response.png)

![](screenshots/home-assistant-integration-2.png)

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

```shell
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
  "index": 0,
  "name": "NVIDIA GeForce RTX 3090",
  "gpu_utilisation": 0,
  "memory_utilisation": 0,
  "power_watts": 22,
  "memory_total_gb": 24,
  "memory_used_gb": 22.44,
  "memory_free_gb": 1.56,
  "memory_usage_percent": 94,
  "temperature": 38,
  "fan_speed": 0,
  "power_limit_watts": 360,
  "processes": [{
    "Pid": 2409765,
    "UsedGpuMemoryMb": 22650,
    "Name": "cuda_v12/ollama_llama_server",
    "Arguments": ["--model", "/models/mixtral", "--ctx-size", "2048", "--batch-size", "512", "--embedding", "--log-disable", "--n-gpu-layers", "26", "--flash-attn", "--parallel", "1", "--port", "39467"]
  }]
}, {
  "index": 1,
  "name": "NVIDIA RTX A4000",
  "gpu_utilisation": 0,
  "memory_utilisation": 0,
  "power_watts": 14,
  "memory_total_gb": 15.99,
  "memory_used_gb": 13.88,
  "memory_free_gb": 2.11,
  "memory_usage_percent": 87,
  "temperature": 35,
  "fan_speed": 41,
  "power_limit_watts": 140,
  "processes": [{
    "Pid": 2409765,
    "UsedGpuMemoryMb": 13934,
    "Name": "cuda_v12/ollama_llama_server",
    "Arguments": ["--model", "/models/mixtral", "--ctx-size", "2048", "--batch-size", "512", "--embedding", "--log-disable", "--n-gpu-layers", "26", "--flash-attn", "--parallel", "1", "--port", "39467"]
  }],
}]
```

### Home Assistant Integration

Example of using the API to integrate with Home Assistant:

```yaml
sensors:

- platform: rest
  name: "NAS GPU Utilisation - RTX3090"
  resource: http://localhost:9999
  unit_of_measurement: "%"
  unique_id: gpu_0
  scan_interval: 30
  json_attributes_path: '$.0'
  json_attributes:
    - name
    - index
    - gpu_utilisation
    - memory_utilisation
    - memory_used_gb
    - memory_free_gb
    - power_watts
    - power_limit_watts
    - temperature
    - fan_speed
    - processes
  value_template: '{{ value_json[0].memory_utilisation }}'

- platform: rest
  name: "NAS GPU Utilisation - RTX A4000"
  resource: http://localhost:9999
  unit_of_measurement: "%"
  unique_id: gpu_1
  scan_interval: 30
  json_attributes_path: '$.1'
  json_attributes:
    - name
    - index
    - gpu_utilisation
    - memory_utilisation
    - memory_used_gb
    - memory_free_gb
    - power_watts
    - power_limit_watts
    - temperature
    - fan_speed
    - processes
  value_template: '{{ value_json[1].memory_utilisation }}'
```

And you might integrate this into a dashboard (as pictured above) like so:

```yaml
title: GPUs
path: gpus
icon: mdi:expansion-card
type: sections
max_columns: 3
sections:
  - type: grid
    cards:
      - type: custom:layout-card
        layout_type: masonry
        layout: {}
        cards:
          - type: gauge
            entity: sensor.nas_gpu_power_rtx3090
            unit: W
            name: RTX3090 Power
            min: 20
            max: 390
            severity:
              green: 30
              yellow: 350
              red: 380
            needle: true
          - type: gauge
            entity: sensor.nas_gpu_temperature_rtx3090
            max: 92
            severity:
              green: 60
              yellow: 80
              red: 90
            needle: true
            min: 0
            unit: ℃
          - type: grid
            columns: 2
            cards:
              - type: gauge
                entity: sensor.nas_gpu_utilisation_rtx3090
                max: 100
                severity:
                  green: 80
                  yellow: 90
                  red: 95
                needle: true
                min: 0
                unit: "%"
              - type: gauge
                entity: sensor.nas_gpu_memory_used_rtx3090
                unit: GB
                max: 24
                severity:
                  green: 20
                  yellow: 22
                  red: 23.9
                needle: true
                min: 0
      - type: custom:apexcharts-card
        header:
          show: true
          title: RTX 3090
          show_states: true
          colorize_states: true
        apex_config:
          chart:
            height: 300px
            update_interval: 2m
        graph_span: 12h
        series:
          - entity: sensor.nas_gpu_utilisation_rtx3090
            stroke_width: 2
          - entity: sensor.nas_gpu_temperature_rtx3090
            stroke_width: 3
          - entity: sensor.nas_gpu_power_rtx3090
            stroke_width: 3
    title: GPU 0 - RTX3090
  - type: grid
    cards:
      - type: custom:layout-card
        layout_type: masonry
        layout: {}
        cards:
          - type: gauge
            entity: sensor.nas_gpu_power_rtx_a4000_a
            unit: W
            name: RTX A4000 A Power
            min: 20
            max: 140
            severity:
              green: 25
              yellow: 120
              red: 130
            needle: true
          - type: gauge
            entity: sensor.nas_gpu_temperature_rtx_a4000_a
            max: 92
            severity:
              green: 60
              yellow: 80
              red: 90
            needle: true
            min: 0
            unit: ℃
          - type: grid
            columns: 2
            cards:
              - type: gauge
                entity: sensor.nas_gpu_utilisation_rtx_a4000_a
                max: 100
                severity:
                  green: 80
                  yellow: 90
                  red: 95
                needle: true
                min: 0
                unit: "%"
              - type: gauge
                entity: sensor.nas_gpu_memory_used_rtx_a4000_a
                unit: GB
                max: 24
                severity:
                  green: 20
                  yellow: 22
                  red: 23.9
                needle: true
                min: 0
      - type: custom:apexcharts-card
        header:
          show: true
          title: RTX A4000 A
          show_states: true
          colorize_states: true
        apex_config:
          chart:
            height: 300px
            update_interval: 2m
        graph_span: 12h
        series:
          - entity: sensor.nas_gpu_1_power_watts
            stroke_width: 3
          - entity: sensor.nas_gpu_utilisation_rtx_a4000_a
            stroke_width: 2
          - entity: sensor.nas_gpu_temperature_rtx_a4000_a
            stroke_width: 3
    title: GPU 1 - RTX A4000 A
  - type: grid
    cards:
      - type: custom:layout-card
        layout_type: masonry
        layout: {}
        cards:
          - type: gauge
            entity: sensor.nas_gpu_power_rtx_a4000_b
            unit: W
            name: RTX A4000 B Power
            min: 20
            max: 140
            severity:
              green: 25
              yellow: 120
              red: 130
            needle: true
          - type: gauge
            entity: sensor.nas_gpu_temperature_rtx_a4000_b
            max: 92
            severity:
              green: 60
              yellow: 80
              red: 90
            needle: true
            min: 0
            unit: ℃
          - type: grid
            columns: 2
            cards:
              - type: gauge
                entity: sensor.nas_gpu_utilisation_rtx_a4000_b_2
                max: 100
                severity:
                  green: 80
                  yellow: 90
                  red: 95
                needle: true
                min: 0
                unit: "%"
              - type: gauge
                entity: sensor.nas_gpu_memory_used_rtx_a4000_b
                unit: GB
                max: 24
                severity:
                  green: 20
                  yellow: 22
                  red: 23.9
                needle: true
                min: 0
      - type: custom:apexcharts-card
        header:
          show: true
          title: RTX A4000 B
          show_states: true
          colorize_states: true
        apex_config:
          chart:
            height: 300px
            update_interval: 2m
        graph_span: 12h
        series:
          - entity: sensor.nas_gpu_utilisation_rtx_a4000_b
            stroke_width: 2
          - entity: sensor.nas_gpu_temperature_rtx_a4000_b
            stroke_width: 3
    title: GPU 2 - RTX A4000 B
cards: []
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
