package cmd

import (
	"bytes"
	"fmt"
	"github.com/asecurityteam/rolling"
	"github.com/markusressel/fan2go/internal"
	"github.com/markusressel/fan2go/internal/configuration"
	"github.com/markusressel/fan2go/internal/fans"
	"github.com/markusressel/fan2go/internal/hwmon"
	"github.com/markusressel/fan2go/internal/sensors"
	"github.com/markusressel/fan2go/internal/ui"
	"github.com/markusressel/fan2go/internal/util"
	"github.com/mgutz/ansi"
	"github.com/spf13/cobra"
	"github.com/tomlazar/table"
	"path/filepath"
	"regexp"
	"strconv"
)

var detectCmd = &cobra.Command{
	Use:   "detect",
	Short: "Detect devices",
	Long:  `Detects all fans and sensors and prints them as a list`,
	Run: func(cmd *cobra.Command, args []string) {
		configuration.LoadConfig()

		controllers, err := internal.FindControllers()
		if err != nil {
			ui.Fatal("Error detecting devices: %v", err)
		}

		// === Print detected devices ===
		tableConfig := &table.Config{
			ShowIndex:       false,
			Color:           !noColor,
			AlternateColors: true,
			TitleColorCode:  ansi.ColorCode("white+buf"),
			AltColorCodes: []string{
				ansi.ColorCode("white"),
				ansi.ColorCode("white:236"),
			},
		}

		for _, controller := range controllers {
			if len(controller.Name) <= 0 {
				continue
			}

			fanList := createFans(controller.Path)
			sensorList := createSensors(controller.Path)

			if len(fanList) <= 0 && len(sensorList) <= 0 {
				continue
			}

			ui.Printfln("> %s", controller.Name)

			var fanRows [][]string
			for _, fan := range fanList {
				pwm := fan.GetPwm()
				rpm := fan.GetRpm()
				isAuto, _ := fan.IsPwmAuto()
				fanRows = append(fanRows, []string{
					"", strconv.Itoa(fan.Index), fan.Label, strconv.Itoa(rpm), strconv.Itoa(pwm), fmt.Sprintf("%v", isAuto),
				})
			}
			var fanHeaders = []string{"Fans   ", "Index", "Label", "RPM", "PWM", "Auto"}

			fanTable := table.Table{
				Headers: fanHeaders,
				Rows:    fanRows,
			}

			var sensorRows [][]string
			for _, sensor := range sensorList {
				value, _ := sensor.GetValue()

				sensorRows = append(sensorRows, []string{
					"", strconv.Itoa(sensor.Index), sensor.Label, strconv.Itoa(int(value)),
				})
			}
			var sensorHeaders = []string{"Sensors", "Index", "Label", "Value"}

			sensorTable := table.Table{
				Headers: sensorHeaders,
				Rows:    sensorRows,
			}

			tables := []table.Table{fanTable, sensorTable}

			for idx, table := range tables {
				if table.Rows == nil {
					continue
				}
				var buf bytes.Buffer
				tableErr := table.WriteTable(&buf, tableConfig)
				if tableErr != nil {
					ui.Fatal("Error printing table: %v", tableErr)
				}
				tableString := buf.String()
				if idx < (len(tables) - 1) {
					ui.Printf(tableString)
				} else {
					ui.Printfln(tableString)
				}
			}
		}
	},
}

// creates fan objects for the given device path
func createFans(devicePath string) (fanList []*fans.HwMonFan) {
	rpmInputs := map[int]string{}
	pwmOutputs := map[int]string{}

	numbersRegex := regexp.MustCompile("\\d+")

	inputPaths := util.FindFilesMatching(devicePath, hwmon.FanInputRegex)
	for _, path := range inputPaths {
		_, fileName := filepath.Split(path)
		index, err := strconv.Atoi(numbersRegex.FindString(fileName))
		if err != nil {
			continue
		}
		rpmInputs[index] = path
	}

	pwmPaths := util.FindFilesMatching(devicePath, hwmon.PwmRegex)
	for _, path := range pwmPaths {
		_, fileName := filepath.Split(path)
		index, err := strconv.Atoi(numbersRegex.FindString(fileName))
		if err != nil {
			continue
		}
		pwmOutputs[index] = path
	}

	for idx, output := range pwmOutputs {
		_, inputFile := filepath.Split(output)

		input, _ := rpmInputs[idx]

		label := util.GetLabel(devicePath, inputFile)

		fan := &fans.HwMonFan{
			Label:        label,
			Index:        idx,
			PwmOutput:    output,
			RpmInput:     input,
			RpmMovingAvg: 0,
			MinPwm:       fans.MinPwmValue,
			MaxPwm:       fans.MaxPwmValue,
			FanCurveData: &map[int]*rolling.PointPolicy{},
			LastSetPwm:   fans.InitialLastSetPwm,
		}

		// store original pwm_enable value
		pwmEnabled, err := fan.GetPwmEnabled()
		if err != nil {
			ui.Fatal("Cannot read pwm_enable value of %s", fan.GetId())
		}
		fan.OriginalPwmEnabled = pwmEnabled

		fanList = append(fanList, fan)
	}

	return fanList
}

// creates sensor objects for the given device path
func createSensors(devicePath string) (result []*sensors.HwmonSensor) {
	inputs := util.FindFilesMatching(devicePath, hwmon.TempInputRegex)

	for _, input := range inputs {
		_, file := filepath.Split(input)
		label := util.GetLabel(devicePath, file)

		index, err := strconv.Atoi(string(file[4]))
		if err != nil {
			ui.Fatal("%v", err)
		}

		sensor := &sensors.HwmonSensor{
			Label: label,
			Index: index,
			Input: input,
		}
		result = append(result, sensor)
	}

	return result
}

func init() {
	rootCmd.AddCommand(detectCmd)
}
