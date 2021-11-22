package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/asecurityteam/rolling"
	"github.com/markusressel/fan2go/internal/configuration"
	"github.com/markusressel/fan2go/internal/fans"
	"github.com/markusressel/fan2go/internal/sensors"
	"github.com/markusressel/fan2go/internal/ui"
	"github.com/markusressel/fan2go/internal/util"
	"github.com/oklog/run"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	MaxPwmValue       = 255
	MinPwmValue       = 0
	InitialLastSetPwm = -10
)

var (
	SensorMap = map[string]Sensor{}
	FanMap    = map[string]Fan{}
)

func Run() {
	if getProcessOwner() != "root" {
		ui.Fatal("Fan control requires root permissions to be able to modify fan speeds, please run fan2go as root")
	}

	persistence := NewPersistence(configuration.CurrentConfig.DbPath)

	controllers, err := FindControllers()
	if err != nil {
		ui.Fatal("Error detecting devices: %s", err.Error())
	}
	MapConfigToControllers(controllers)
	for _, curveConfig := range configuration.CurrentConfig.Curves {
		NewSpeedCurve(curveConfig)
	}

	ctx, cancel := context.WithCancel(context.Background())

	var g run.Group
	{
		// === sensor monitoring
		for _, controller := range controllers {
			for _, s := range controller.Sensors {
				if s.GetConfig() == nil {
					ui.Info("Ignoring unconfigured sensor %s/%s", controller.Name, s.GetLabel())
					continue
				}

				pollingRate := configuration.CurrentConfig.TempSensorPollingRate
				mon := NewSensorMonitor(s, pollingRate)

				g.Add(func() error {
					return mon.Run(ctx)
				}, func(err error) {
					ui.Fatal("Error monitoring sensor: %v", err)
				})
			}
		}
	}
	{
		// === fan controllers
		count := 0
		for _, controller := range controllers {
			for _, f := range controller.Fans {
				fan := f
				if fan.GetConfig() == nil {
					// this fan is not configured, ignore it
					ui.Info("Ignoring unconfigured fan %s/%s", controller.Name, fan.GetName())
					continue
				}

				fanId := fan.GetConfig().Id

				updateRate := configuration.CurrentConfig.ControllerAdjustmentTickRate
				fanController := NewFanController(persistence, fan, updateRate)

				g.Add(func() error {
					rpmTick := time.Tick(configuration.CurrentConfig.RpmPollingRate)
					return rpmMonitor(ctx, fanId, rpmTick)
				}, func(err error) {
					// nothing to do here
				})

				g.Add(func() error {
					return fanController.Run(ctx)
				}, func(err error) {
					if err != nil {
						ui.Error("Something went wrong: %v", err)
					}

					ui.Info("Trying to restore fan settings for %s...", fanId)

					// TODO: move this error handling to the FanController implementation

					// try to reset the pwm_enable value
					if fan.GetOriginalPwmEnabled() != 1 {
						err := fan.SetPwmEnabled(fan.GetOriginalPwmEnabled())
						if err == nil {
							return
						}
					}
					err = setPwm(fan, MaxPwmValue)
					if err != nil {
						ui.Warning("Unable to restore fan %s, make sure it is running!", fan.GetConfig().Id)
					}
				})
				count++
			}
		}

		if count == 0 {
			ui.Fatal("No valid fan configurations, exiting.")
		}
	}
	{
		sig := make(chan os.Signal)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM, os.Kill)

		g.Add(func() error {
			<-sig
			ui.Info("Exiting...")
			return nil
		}, func(err error) {
			cancel()
			close(sig)
		})
	}

	if err := g.Run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func rpmMonitor(ctx context.Context, fanId string, tick <-chan time.Time) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick:
			measureRpm(fanId)
		}
	}
}

func getProcessOwner() string {
	stdout, err := exec.Command("ps", "-o", "user=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		ui.Error("%v", err)
		os.Exit(1)
	}
	return strings.TrimSpace(string(stdout))
}

// Map detect devices to configuration values
func MapConfigToControllers(controllers []*HwMonController) {
	for _, controller := range controllers {
		// match fan and fan config entries
		for _, fan := range controller.Fans {
			fanConfig := findFanConfig(controller, fan)
			if fanConfig != nil {
				ui.Debug("Mapping fan config %s to %s", fanConfig.Id, fan.(*fans.HwMonFan).PwmOutput)
				fan.SetConfig(fanConfig)
				FanMap[fanConfig.Id] = fan
			}
		}
		// match sensor and sensor config entries
		for _, sensor := range controller.Sensors {
			sensorConfig := findSensorConfig(controller, sensor)
			if sensorConfig == nil {
				continue
			}

			ui.Debug("Mapping sensor config %s to %s", sensorConfig.Id, sensor.(*sensors.HwmonSensor).Input)

			sensor.SetConfig(sensorConfig)
			// remember ID -> Sensor association for later
			SensorMap[sensorConfig.Id] = sensor

			// initialize arrays for storing temps
			currentValue, err := sensor.GetValue()
			if err != nil {
				ui.Fatal("Error reading sensor %s: %v", sensorConfig.Id, err)
			}
			sensor.SetMovingAvg(currentValue)
		}
	}
}

// read the current value of a fan RPM sensor and append it to the moving window
func measureRpm(fanId string) {
	fan := FanMap[fanId]

	pwm := fan.GetPwm()
	rpm := fan.GetRpm()

	ui.Debug("Measured RPM of %d at PWM %d for fan %s", rpm, pwm, fan.GetConfig().Id)

	updatedRpmAvg := updateSimpleMovingAvg(fan.GetRpmAvg(), configuration.CurrentConfig.RpmRollingWindowSize, float64(rpm))
	fan.SetRpmAvg(updatedRpmAvg)

	pwmRpmMap := fan.GetFanCurveData()
	pointWindow, exists := (*pwmRpmMap)[pwm]
	if !exists {
		// create rolling window for current pwm value
		pointWindow = createRollingWindow(configuration.CurrentConfig.RpmRollingWindowSize)
		(*pwmRpmMap)[pwm] = pointWindow
	}
	pointWindow.Append(float64(rpm))
}

// GetPwmBoundaries calculates the startPwm and maxPwm values for a fan based on its fan curve data
func GetPwmBoundaries(fan Fan) (startPwm int, maxPwm int) {
	startPwm = 255
	maxPwm = 255
	pwmRpmMap := fan.GetFanCurveData()

	// get pwm keys that we have data for
	keys := make([]int, len(*pwmRpmMap))
	if pwmRpmMap == nil || len(keys) <= 0 {
		// we have no data yet
		startPwm = 0
	} else {
		i := 0
		for k := range *pwmRpmMap {
			keys[i] = k
			i++
		}
		// sort them increasing
		sort.Ints(keys)

		maxRpm := 0
		for _, pwm := range keys {
			window := (*pwmRpmMap)[pwm]
			avgRpm := int(getWindowAvg(window))

			if avgRpm > maxRpm {
				maxRpm = avgRpm
				maxPwm = pwm
			}

			if avgRpm > 0 && pwm < startPwm {
				startPwm = pwm
			}
		}
	}

	return startPwm, maxPwm
}

// AttachFanCurveData attaches fan curve data from persistence to a fan
// Note: When the given data is incomplete, all values up until the highest
// value in the given dataset will be interpolated linearly
// returns os.ErrInvalid if curveData is void of any data
func AttachFanCurveData(curveData *map[int][]float64, fan Fan) (err error) {
	// convert the persisted map to arrays back to a moving window and attach it to the fan

	if curveData == nil || len(*curveData) <= 0 {
		ui.Error("Cant attach empty fan curve data to fan %s", fan.GetConfig().Id)
		return os.ErrInvalid
	}

	const limit = 255
	var lastValueIdx int
	var lastValueAvg float64
	var nextValueIdx int
	var nextValueAvg float64
	for i := 0; i <= limit; i++ {
		fanCurveMovingWindow := createRollingWindow(configuration.CurrentConfig.RpmRollingWindowSize)

		pointValues, containsKey := (*curveData)[i]
		if containsKey && len(pointValues) > 0 {
			lastValueIdx = i
			lastValueAvg = util.Avg(pointValues)
		} else {
			if pointValues == nil {
				pointValues = []float64{lastValueAvg}
			}
			// find next value in curveData
			nextValueIdx = i
			for j := i; j <= limit; j++ {
				pointValues, containsKey := (*curveData)[j]
				if containsKey {
					nextValueIdx = j
					nextValueAvg = util.Avg(pointValues)
				}
			}
			if nextValueIdx == i {
				// we didn't find a next value in curveData, so we just copy the last point
				var valuesCopy = []float64{}
				copy(pointValues, valuesCopy)
				pointValues = valuesCopy
			} else {
				// interpolate average value to the next existing key
				ratio := util.Ratio(float64(i), float64(lastValueIdx), float64(nextValueIdx))
				interpolation := lastValueAvg + ratio*(nextValueAvg-lastValueAvg)
				pointValues = []float64{interpolation}
			}
		}

		var currentAvg float64
		for k := 0; k < configuration.CurrentConfig.RpmRollingWindowSize; k++ {
			var rpm float64

			if k < len(pointValues) {
				rpm = pointValues[k]
			} else {
				// fill the rolling window with averages if given values are not sufficient
				rpm = currentAvg
			}

			// update average
			if k == 0 {
				currentAvg = rpm
			} else {
				currentAvg = (currentAvg + rpm) / 2
			}

			// add value to window
			fanCurveMovingWindow.Append(rpm)
		}

		data := fan.GetFanCurveData()
		(*data)[i] = fanCurveMovingWindow
	}

	startPwm, maxPwm := GetPwmBoundaries(fan)

	fan.SetStartPwm(startPwm)
	fan.SetMaxPwm(maxPwm)

	// TODO: we don't have a way to determine this yet
	fan.SetMinPwm(startPwm)

	return err
}

func findFanConfig(controller *HwMonController, fan Fan) (fanConfig *configuration.FanConfig) {
	for _, fanConfig := range configuration.CurrentConfig.Fans {

		marshalled, err := json.Marshal(fanConfig.Params)
		if err != nil {
			ui.Error("Couldn't marshal curve configuration: %v", err)
		}

		if fanConfig.Type == configuration.FanTypeHwMon {
			c := configuration.HwMonFanParams{}
			if err := json.Unmarshal(marshalled, &c); err != nil {
				ui.Fatal("Couldn't unmarshal fan parameter configuration: %v", err)
			}
			hwmonFan := fan.(*fans.HwMonFan)

			if controller.Platform == c.Platform &&
				hwmonFan.Index == c.Index {
				return &fanConfig
			}
		} else if fanConfig.Type == configuration.FanTypeFile {
			// TODO
		}
	}
	return nil
}

func findSensorConfig(controller *HwMonController, sensor Sensor) (sensorConfig *configuration.SensorConfig) {
	for _, sensorConfig := range configuration.CurrentConfig.Sensors {

		// TODO: find a way around this marshaling, or move it to a central place
		marshalled, err := json.Marshal(sensorConfig.Params)
		if err != nil {
			ui.Error("Couldn't marshal curve configuration: %v", err)
		}

		if sensorConfig.Type == configuration.SensorTypeHwMon {
			c := configuration.HwMonSensor{}
			if err := json.Unmarshal(marshalled, &c); err != nil {
				ui.Fatal("Couldn't unmarshal sensor parameter configuration: %v", err)
			}

			if controller.Platform == c.Platform &&
				sensor.(*sensors.HwmonSensor).Index == c.Index {
				return &sensorConfig
			}
		} else if sensorConfig.Type == configuration.SensorTypeFile {
			// TODO
		}
	}
	return nil
}

func findPlatform(devicePath string) string {
	platformRegex := regexp.MustCompile(".*/platform/{}/.*")
	return platformRegex.FindString(devicePath)
}

// FindControllers Finds controllers and fans
func FindControllers() (controllers []*HwMonController, err error) {
	hwmonDevices := util.FindHwmonDevicePaths()
	i2cDevices := util.FindI2cDevicePaths()
	allDevices := append(hwmonDevices, i2cDevices...)

	for _, devicePath := range allDevices {

		var deviceName = util.GetDeviceName(devicePath)
		var identifier = computeIdentifier(devicePath, deviceName)

		dType := util.GetDeviceType(devicePath)
		modalias := util.GetDeviceModalias(devicePath)
		platform := findPlatform(devicePath)
		if len(platform) <= 0 {
			platform = identifier
		}

		fanList := createFans(devicePath)
		sensorList := createSensors(devicePath)

		if len(fanList) <= 0 && len(sensorList) <= 0 {
			continue
		}

		controller := &HwMonController{
			Name:     identifier,
			DType:    dType,
			Modalias: modalias,
			Platform: platform,
			Path:     devicePath,
			Fans:     fanList,
			Sensors:  sensorList,
		}
		controllers = append(controllers, controller)
	}

	return controllers, err
}

func computeIdentifier(devicePath string, deviceName string) (name string) {
	pciDeviceRegex := regexp.MustCompile("\\w+:\\w{2}:\\w{2}\\.\\d")

	if len(name) <= 0 {
		name = deviceName
	}

	if len(name) <= 0 {
		_, name = filepath.Split(devicePath)
	}

	if strings.Contains(devicePath, "/pci") {
		// add pci suffix to name
		matches := pciDeviceRegex.FindAllString(devicePath, -1)
		if len(matches) > 0 {
			lastMatch := matches[len(matches)-1]
			pciIdentifier := util.CreateShortPciIdentifier(lastMatch)
			name = fmt.Sprintf("%s-%s", name, pciIdentifier)
		}
	}

	return name
}

// creates fan objects for the given device path
func createFans(devicePath string) (fanList []Fan) {
	inputs := util.FindFilesMatching(devicePath, "^fan[1-9]_input$")
	outputs := util.FindFilesMatching(devicePath, "^pwm[1-9]$")

	for idx, output := range outputs {
		_, file := filepath.Split(output)

		label := util.GetLabel(devicePath, output)

		index, err := strconv.Atoi(file[len(file)-1:])
		if err != nil {
			ui.Fatal("%v", err)
		}

		fan := &fans.HwMonFan{
			Name:         file,
			Label:        label,
			Index:        index,
			PwmOutput:    output,
			RpmInput:     inputs[idx],
			RpmMovingAvg: 0,
			MinPwm:       MinPwmValue,
			MaxPwm:       MaxPwmValue,
			FanCurveData: &map[int]*rolling.PointPolicy{},
			LastSetPwm:   InitialLastSetPwm,
		}

		// store original pwm_enable value
		pwmEnabled, err := fan.GetPwmEnabled()
		if err != nil {
			ui.Fatal("Cannot read pwm_enable value of %s", fan.GetConfig().Id)
		}
		fan.OriginalPwmEnabled = pwmEnabled

		fanList = append(fanList, fan)
	}

	return fanList
}

// creates sensor objects for the given device path
func createSensors(devicePath string) (result []Sensor) {
	inputs := util.FindFilesMatching(devicePath, "^temp[1-9]_input$")

	for _, input := range inputs {
		_, file := filepath.Split(input)
		label := util.GetLabel(devicePath, file)

		index, err := strconv.Atoi(string(file[4]))
		if err != nil {
			ui.Fatal("%v", err)
		}

		sensor := &sensors.HwmonSensor{
			Name:  file,
			Label: label,
			Index: index,
			Input: input,
		}
		result = append(result, sensor)
	}

	return result
}

func createRollingWindow(size int) *rolling.PointPolicy {
	return rolling.NewPointPolicy(rolling.NewWindow(size))
}

// returns the average of all values in the window
func getWindowAvg(window *rolling.PointPolicy) float64 {
	return window.Reduce(rolling.Avg)
}
