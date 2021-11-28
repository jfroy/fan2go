package internal

import (
	"context"
	"fmt"
	"github.com/markusressel/fan2go/internal/configuration"
	"github.com/markusressel/fan2go/internal/controller"
	"github.com/markusressel/fan2go/internal/curves"
	"github.com/markusressel/fan2go/internal/fans"
	"github.com/markusressel/fan2go/internal/hwmon"
	"github.com/markusressel/fan2go/internal/persistence"
	"github.com/markusressel/fan2go/internal/sensors"
	"github.com/markusressel/fan2go/internal/ui"
	"github.com/markusressel/fan2go/internal/util"
	"github.com/oklog/run"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func Run() {
	if getProcessOwner() != "root" {
		ui.Fatal("Fan control requires root permissions to be able to modify fan speeds, please run fan2go as root")
	}

	pers := persistence.NewPersistence(configuration.CurrentConfig.DbPath)

	InitializeObjects()

	ctx, cancel := context.WithCancel(context.Background())

	var g run.Group
	{
		// === sensor monitoring
		for _, sensor := range sensors.SensorMap {
			pollingRate := configuration.CurrentConfig.TempSensorPollingRate
			mon := NewSensorMonitor(sensor, pollingRate)

			g.Add(func() error {
				return mon.Run(ctx)
			}, func(err error) {
				ui.Fatal("Error monitoring sensor: %v", err)
			})
		}
	}
	{
		// === fan controllers
		for fanId, fan := range fans.FanMap {
			updateRate := configuration.CurrentConfig.ControllerAdjustmentTickRate
			fanController := controller.NewFanController(pers, fan, updateRate)

			g.Add(func() error {
				rpmTick := time.Tick(configuration.CurrentConfig.RpmPollingRate)
				return rpmMonitor(ctx, fanId, rpmTick)
			}, func(err error) {
				ui.Error("Something went wrong: %v", err)
			})

			g.Add(func() error {
				return fanController.Run(ctx)
			}, func(err error) {
				ui.Error("Something went wrong: %v", err)
			})
		}

		if len(fans.FanMap) == 0 {
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

func InitializeObjects() {
	controllers, err := FindControllers()
	if err != nil {
		ui.Fatal("Error detecting devices: %s", err.Error())
	}

	for _, config := range configuration.CurrentConfig.Sensors {
		sensor, err := sensors.NewSensor(config, controllers)
		if err != nil {
			ui.Fatal("Unable to process curve configuration: %s", config.ID)
		}

		currentValue, err := sensor.GetValue()
		if err != nil {
			ui.Fatal("Error reading sensor %s: %v", config.ID, err)
		}
		sensor.SetMovingAvg(currentValue)

		sensors.SensorMap[config.ID] = sensor
	}

	for _, config := range configuration.CurrentConfig.Curves {
		curve, err := curves.NewSpeedCurve(config)
		if err != nil {
			ui.Fatal("Unable to process curve configuration: %s", config.ID)
		}
		curves.SpeedCurveMap[config.ID] = curve
	}

	for _, config := range configuration.CurrentConfig.Fans {
		if config.HwMon != nil {
			for _, c := range controllers {
				if c.Platform == config.HwMon.Platform {
					config.HwMon.PwmOutput = c.PwmInputs[config.HwMon.Index]
					config.HwMon.RpmInput = c.FanInputs[config.HwMon.Index]
					break
				}
			}
		}

		fan, err := fans.NewFan(config)
		if err != nil {
			ui.Fatal("Unable to process fan configuration: %s", config.ID)
		}
		fans.FanMap[config.ID] = fan
	}

}

func rpmMonitor(ctx context.Context, fanId string, tick <-chan time.Time) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick:
			// TODO:
			//measureRpm(fanId)
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

// FindControllers finds hwmon controllers
func FindControllers() (controllers []*hwmon.HwMonController, err error) {
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

		fanInputs := util.FindFilesMatching(devicePath, hwmon.FanInputRegex)
		pwmInputs := util.FindFilesMatching(devicePath, hwmon.PwmRegex)
		tempInputs := util.FindFilesMatching(devicePath, hwmon.TempInputRegex)

		if len(fanInputs) <= 0 && len(pwmInputs) <= 0 && len(tempInputs) <= 0 {
			continue
		}

		c := &hwmon.HwMonController{
			Name:       identifier,
			DType:      dType,
			Modalias:   modalias,
			Platform:   platform,
			Path:       devicePath,
			TempInputs: tempInputs,
			PwmInputs:  pwmInputs,
			FanInputs:  fanInputs,
		}
		controllers = append(controllers, c)
	}

	return controllers, err
}

func findPlatform(devicePath string) string {
	platformRegex := regexp.MustCompile(".*/platform/{}/.*")
	return platformRegex.FindString(devicePath)
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
