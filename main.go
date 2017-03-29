package main

import (
	"fmt"
	"github.com/spf13/viper"
	"log"
	"os"
	"github.com/spf13/cast"
	"time"
	"sync"
	"net/http"
	"io/ioutil"
	"encoding/json"
)

type Device struct {
	Id         string
	Ip         string
	CheckEvery int
}

type EcoDevicesResponse struct {
	T1Base int `json:"T1_BASE"`
	T1Papp int `json:"T1_PAPP"`
	T2Base int `json:"T2_BASE"`
	T2Papp int `json:"T2_PAPP"`
}

type MQTTData struct {
	T1Index int
	T1IndexDay int
	T1IndexMonth int
	T1IndexYear int
	T1Power int
}

func initConfig() () {
	viper.SetConfigName("app")
	viper.AddConfigPath("config")

	err := viper.ReadInConfig()
	if err != nil {
		log.Fatal(err);
		os.Exit(1)
	}
}

func getEcoDevicesData(device *Device) (*EcoDevicesResponse, error) {
	ecoDevicesClient := http.Client{
		Timeout: time.Second * 20,
	}

	req, err := http.NewRequest(http.MethodGet, "http://" + device.Ip + "/api/xdevices.json?cmd=10", nil)
	if err != nil {
		return nil, err
	}

	res, getErr := ecoDevicesClient.Do(req)
	if getErr != nil {
		return nil, getErr
	}

	body, readErr := ioutil.ReadAll(res.Body)
	if readErr != nil {
		return nil, readErr
	}

	var ecoDevicesResponse EcoDevicesResponse
	jsonErr := json.Unmarshal(body, &ecoDevicesResponse)
	if jsonErr != nil {
		return nil, jsonErr
	}
	return &ecoDevicesResponse, nil
}

func getEcoDevicesDataRoutine(device *Device, wg *sync.WaitGroup) {
	defer wg.Done()

	for {
		ecoDevicesResponse, ecoDevicesErr := getEcoDevicesData(device)
		if (ecoDevicesErr != nil) {
			log.Fatal(ecoDevicesErr);
		}
		fmt.Println(device.Id);
		fmt.Println(ecoDevicesResponse);
		time.Sleep(time.Duration(device.CheckEvery) * time.Second)
	}
}

func initDevices(wg *sync.WaitGroup) {
	devices, devicesErr := viper.Get("eco-devices").([]interface{})
	if !devicesErr {
		log.Fatal("please specify at least one eco-devices in config");
		os.Exit(1)
	} else {
		wg.Add(len(devices))
		for index, table := range devices {
			if device, deviceErr := table.(map[string]interface{}); deviceErr {
				if (cast.ToString(device["id"]) == "") {
					device["id"] = "eco-devices-" + cast.ToString(index)
				}

				if (cast.ToString(device["ip"]) == "") {
					log.Fatal("please specify an ip address for '" + cast.ToString(device["id"]) + "' device in config");
					os.Exit(1)
				}

				if (cast.ToInt(device["check-every"]) == 0) {
					device["check-every"] = 30
				}

				go getEcoDevicesDataRoutine(&Device{
					Id: cast.ToString(device["id"]),
					Ip: cast.ToString(device["ip"]),
					CheckEvery: cast.ToInt(device["check-every"]),
				}, wg)
			}
		}
	}
}

func main() {
	var wg sync.WaitGroup

	initConfig()
	initDevices(&wg)

	wg.Wait()
}
