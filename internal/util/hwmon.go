package util

import (
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
)

// GetDeviceName read the name of a device
func GetDeviceName(devicePath string) string {
	namePath := devicePath + "/name"
	content, _ := ioutil.ReadFile(namePath)
	name := string(content)
	if len(name) <= 0 {
		_, name = filepath.Split(devicePath)
	}
	return strings.TrimSpace(name)
}

// GetLabel read the label of a in/output of a device
func GetLabel(devicePath string, input string) string {
	labelPath := strings.TrimSuffix(devicePath+"/"+input, "input") + "label"

	content, _ := ioutil.ReadFile(labelPath)
	label := string(content)
	if len(label) <= 0 {
		_, label = filepath.Split(devicePath)
	}
	return strings.TrimSpace(label)
}

// GetDeviceModalias read the modalias of a device
func GetDeviceModalias(devicePath string) string {
	modaliasPath := devicePath + "/device/modalias"
	content, _ := ioutil.ReadFile(modaliasPath)
	return strings.TrimSpace(string(content))
}

// GetDeviceType read the type of a device
func GetDeviceType(devicePath string) string {
	modaliasPath := devicePath + "/device/type"
	content, _ := ioutil.ReadFile(modaliasPath)
	return strings.TrimSpace(string(content))
}

func FindI2cDevicePaths() []string {
	basePath := "/sys/bus/i2c/devices"

	if _, err := os.Stat(basePath); err != nil {
		if os.IsNotExist(err) {
			// file.go does not exist
		} else {
			// other error
		}
		return []string{}
	}

	return FindFilesMatching(basePath, ".+-.+")

	//	# Find available fan control outputs
	//	MATCH=$device/'pwm[1-9]'
	//	device_pwm=$(echo $MATCH)
	//	if [ "$SYSFS" = "1" -a "$MATCH" = "$device_pwm" ]
	//	then
	//		# Deprecated naming scheme (used in kernels 2.6.5 to 2.6.9)
	//		MATCH=$device/'fan[1-9]_pwm'
	//		device_pwm=$(echo $MATCH)
	//	fi
	//	if [ "$MATCH" != "$device_pwm" ]
	//	then
	//		PWM="$PWM $device_pwm"
	//	fi
}

func FindHwmonDevicePaths() []string {
	basePath := "/sys/class/hwmon"
	if _, err := os.Stat(basePath); err != nil {
		if os.IsNotExist(err) {
			// file.go does not exist
		} else {
			// other error
		}
		return []string{}
	}

	result := FindFilesMatching(basePath, "hwmon.*")

	return result
}
