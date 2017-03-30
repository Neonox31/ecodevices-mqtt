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
	"github.com/boltdb/bolt"
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
	T1Price float64
	T1PriceDay float64
	T1PriceMonth float64
	T1PriceYear float64
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

func getDayIndex(device *Device, input string) (int, error) {
	if (viper.GetString("db.path") == "") {
		log.Fatal("please specify a db path in config");
		os.Exit(1)
	}
	db, err := bolt.Open(viper.GetString("db.path"), 0600, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	tx, err := db.Begin(true)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.CreateBucketIfNotExists([]byte(device.Id + "-indexes"))
	if err != nil {
		log.Fatal(err)
	}

	db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(device.Id + "-indexes"))
		v := b.Get([]byte(input + "-day"))
		fmt.Printf("The answer is: %s\n", v)
		return nil
	})

	if err := tx.Commit(); err != nil {
		return err
	}
}

func buildMQTTData(device *Device, ecoDevicesData *EcoDevicesResponse) {
	return &MQTTData{
		T1Index: ecoDevicesData.T1Base,
		T1IndexDay: getDayIndex(device, "t1"),
	}
}

func main() {
	var wg sync.WaitGroup

	initConfig()
	initDevices(&wg)

	wg.Wait()
}
