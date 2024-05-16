package main

import (
	"encoding/json"
	"fmt"
	"image/color"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

type GPUInfo struct {
	GPUUtilisation     float64 `json:"gpu_utilisation"`
	MemoryUtilisation  float64 `json:"memory_utilisation"`
	PowerWatts         float64 `json:"power_watts"`
	MemoryTotalGB      float64 `json:"memory_total_gb"`
	MemoryUsedGB       float64 `json:"memory_used_gb"`
	MemoryFreeGB       float64 `json:"memory_free_gb"`
	MemoryUsagePercent float64 `json:"memory_usage_percent"`
	Temperature        float64 `json:"temperature"`
	FanSpeed           float64 `json:"fan_speed"`
	Processes          []struct {
		Pid             int      `json:"Pid"`
		UsedGpuMemoryMb float64  `json:"UsedGpuMemoryMb"`
		Name            string   `json:"Name"`
		Arguments       []string `json:"Arguments"`
	} `json:"processes"`
}

var (
	apiURL   = getEnv("GPU_API_URL", "http://localhost:9999")
	interval = time.Duration(getEnvAsInt("GPU_UPDATE_INTERVAL", 3)) * time.Second
)

func getEnv(key, defaultValue string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return defaultValue
}

func getEnvAsInt(name string, defaultVal int) int {
	if valueStr := getEnv(name, ""); valueStr != "" {
		if value, err := strconv.Atoi(valueStr); err == nil {
			return value
		}
	}
	return defaultVal
}

func fetchGPUInfo() (*GPUInfo, error) {
	resp, err := http.Get(apiURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var gpuInfos []GPUInfo
	if err := json.Unmarshal(body, &gpuInfos); err != nil {
		return nil, err
	}

	if len(gpuInfos) == 0 {
		return nil, fmt.Errorf("no GPU info available")
	}

	return &gpuInfos[0], nil
}

func processNamesAndMemoryUsage(gpuInfo *GPUInfo) [][]string {
	// we want the process name and memory usage in MB (e.g. "ollama_llama_server: 9678")
	var data [][]string
	for _, process := range gpuInfo.Processes {
		data = append(data, []string{process.Name, fmt.Sprintf("%.2f", process.UsedGpuMemoryMb)})
	}
	return data
}

func flameColor(value float64) color.Color {
	r := uint8(255 * value)
	b := uint8(255 * (1 - value))
	return color.RGBA{R: r, G: 0, B: b, A: 255}
}

type FlameProgressBar struct {
	widget.BaseWidget
	value float64
}

func NewFlameProgressBar() *FlameProgressBar {
	bar := &FlameProgressBar{}
	bar.ExtendBaseWidget(bar)
	return bar
}

func (b *FlameProgressBar) CreateRenderer() fyne.WidgetRenderer {
	background := canvas.NewRectangle(theme.ShadowColor())
	bar := canvas.NewRectangle(color.NRGBA{R: 0, G: 0, B: 0xFF, A: 0xFF})

	objects := []fyne.CanvasObject{background, bar}
	renderer := &flameProgressBarRenderer{
		bar:        bar,
		background: background,
		objects:    objects,
	}
	renderer.Refresh()
	return renderer
}

type flameProgressBarRenderer struct {
	bar, background *canvas.Rectangle
	objects         []fyne.CanvasObject
}

func (r *flameProgressBarRenderer) Layout(size fyne.Size) {
	r.background.Resize(size)
	r.bar.Resize(fyne.NewSize(size.Width*float32(r.bar.FillColor.(color.RGBA).R)/255, size.Height))
}

func (r *flameProgressBarRenderer) MinSize() fyne.Size {
	return r.background.MinSize()
}

func (r *flameProgressBarRenderer) Refresh() {
	r.bar.FillColor = flameColor(float64(r.bar.Size().Width) / float64(r.background.Size().Width))
	r.Layout(r.background.Size())
	canvas.Refresh(r.bar)
}

func (r *flameProgressBarRenderer) Objects() []fyne.CanvasObject {
	return r.objects
}

func (r *flameProgressBarRenderer) Destroy() {}

func (b *FlameProgressBar) SetValue(val float64) {
	b.value = val
	b.Refresh()
}

func (b *FlameProgressBar) Value() float64 {
	return b.value
}

func main() {
	a := app.NewWithID("com.sammcj.NVApi-Tray")
	w := a.NewWindow("GPU Info")

	// UI elements
	statusLabel := widget.NewLabel("Fetching GPU info...")
	lastUpdatedLabel := widget.NewLabel("")
	gpuUtilBar := widget.NewProgressBar()
	memUtilBar := widget.NewProgressBar()
	powerBar := widget.NewProgressBar()
	tempBar := widget.NewProgressBar()
	fanSpeedLabel := widget.NewLabel("")

	// Container for the UI elements
	infoContent := container.NewVBox(
		statusLabel,
		lastUpdatedLabel,
		widget.NewLabel("GPU Utilisation:"),
		gpuUtilBar,
		widget.NewLabel("Memory Utilisation:"),
		memUtilBar,
		widget.NewLabel("Power:"),
		powerBar,
		widget.NewLabel("Temperature:"),
		tempBar,
		fanSpeedLabel,
	)

	processData := processNamesAndMemoryUsage(&GPUInfo{})
	processTable := widget.NewTable(
			// Define the number of rows and columns based on the data
			func() (int, int) {
					return len(processData), 2
			},
			// Create a new row widget for each cell
			func() fyne.CanvasObject {
					return NewProcessRow("")
			},
			// Populate data into each cell
			func(i widget.TableCellID, o fyne.CanvasObject) {
					row := o.(*ProcessRow)
					// Cleanup the process name
					procPath := processData[i.Row][0]
					procName := procPath[strings.LastIndex(procPath, "/")+1:]
					// Set the name and memory usage for each row
					if i.Col == 0 {
							row.SetName(procName)
					} else {
							row.SetMemoryUsage(processData[i.Row][1])
					}
			},
	)

	// Set a fixed height for the process table
	processContainer := container.NewVBox(
			processTable,
	)

	// Main content layout with the process table
	mainContent := container.NewVBox(infoContent, widget.NewLabel("Processes:"), processContainer)


	w.SetContent(mainContent)
	w.Resize(fyne.NewSize(600, 600))

	go func() {
		for range time.Tick(interval) {
			gpuInfo, err := fetchGPUInfo()
			if err != nil {
				statusLabel.SetText(fmt.Sprintf("Error: %v", err))
				continue
			}

			statusLabel.SetText("GPU Info Updated")
			lastUpdatedLabel.SetText(fmt.Sprintf("Last Updated: %s (Interval: %s)", time.Now().Format("2006-01-02 15:04:05"), interval))

			gpuUtilBar.SetValue(gpuInfo.GPUUtilisation / 100)
			memUtilBar.SetValue(gpuInfo.MemoryUtilisation / 100)
			powerBar.SetValue(gpuInfo.PowerWatts / 100)
			tempBar.SetValue(gpuInfo.Temperature / 100)

			fanSpeedLabel.SetText(fmt.Sprintf("Fan Speed: %.2f%%", gpuInfo.FanSpeed))

			// Update process data
			processData = processNamesAndMemoryUsage(gpuInfo)
			// correctly size the table to the number of processes
			processTable.Resize(fyne.NewSize(600, float32(30*len(processData))))


			// Refresh the process table with new data
			processTable.Refresh()

			fmt.Printf("Updated process data: %+v\n", processData) // Debug output to ensure data is fetched and processed
		}
	}()

	w.SetMaster()
	w.ShowAndRun()
}

// ProcessRow represents a custom row for the process table
type ProcessRow struct {
	widget.BaseWidget
	memoryUsageBar *FlameProgressBar
	nameLabel      *widget.Label
}

func NewProcessRow(name string) *ProcessRow {
	row := &ProcessRow{
		memoryUsageBar: NewFlameProgressBar(),
		nameLabel:      widget.NewLabel(""),
	}
	row.ExtendBaseWidget(row)
	row.update(name, "0")
	return row
}

func (r *ProcessRow) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(container.NewHBox(
		container.NewStack(r.memoryUsageBar),
		r.nameLabel,
	))
}

func (r *ProcessRow) update(name, memory string) {
	r.SetName(name)
	r.SetMemoryUsage(memory)
	r.Refresh()
}

func (r *ProcessRow) SetMemoryUsage(memory string) {
	usage, err := strconv.ParseFloat(memory, 64)
	if err == nil {
		r.memoryUsageBar.SetValue(usage / 10000)
	}
}

func (r *ProcessRow) SetName(name string) {
	r.nameLabel.SetText(name)
}
