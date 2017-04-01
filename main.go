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
	"errors"
	"strconv"
	"github.com/yosssi/gmq/mqtt"
	"github.com/yosssi/gmq/mqtt/client"
)

type Device struct {
	Id         string
	Ip         string
	CheckEvery int
}

type EcoDevicesInput struct {
	Id    string
	Value int
}

type EcoDevicesResponse struct {
	T1Base int `json:"T1_BASE"`
	T1Papp int `json:"T1_PAPP"`
	T2Base int `json:"T2_BASE"`
	T2Papp int `json:"T2_PAPP"`
}

type MQTTData struct {
	T1Index      int
	T1IndexDay   int
	T1IndexMonth int
	T1IndexYear  int
	T1Power      int
	T1Price      float64
	T1PriceDay   float64
	T1PriceMonth float64
	T1PriceYear  float64
}

func initConfig() () {
	viper.SetConfigName("app")
	viper.AddConfigPath("config")

	err := viper.ReadInConfig()
	if err != nil {
		log.Fatal(err);
		os.Exit(1)
	}

	if (viper.GetString("db.path") == "") {
		log.Fatal("please specify a db path in config");
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

func saveIndex(device *Device, input string, index int) (error) {
	db, err := bolt.Open(viper.GetString("db.path"), 0600, nil)
	if err != nil {
		return err
	}
	defer db.Close()

	db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(device.Id + "-" + input + "-indexes"))
		if (err != nil) {
			return err
		}
		err = b.Put([]byte(time.Now().Format(time.RFC3339)), []byte(cast.ToString(index)))
		if (err != nil) {
			return err
		}
		return nil
	})
	return nil
}

func saveIndexes(device *Device, inputs []EcoDevicesInput) (error) {
	for _, input := range inputs {
		if (input.Value != 0) {
			err := saveIndex(device, input.Id, input.Value)
			if (err != nil) {
				return err
			}
		}
	}

	return nil
}

func getIndexFromStartDate(device *Device, input string, startDate *time.Time) (int, error) {
	var dayIndex int
	bucketName := device.Id + "-" + input + "-indexes"

	db, err := bolt.Open(viper.GetString("db.path"), 0600, nil)
	if err != nil {
		return 0, err
	}
	defer db.Close()

	tErr := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketName))

		if (b == nil) {
			return errors.New("unable to find bucket " + bucketName)
		}

		c := b.Cursor()
		_, startIndexStr := c.Seek([]byte(startDate.Format(time.RFC3339)));
		_, currentIndexStr := c.Last();

		currentIndex, err := strconv.Atoi(string(currentIndexStr))
		if (err != nil) {
			return err
		}
		startIndex, err := strconv.Atoi(string(startIndexStr))
		if (err != nil) {
			return err
		}
		dayIndex = currentIndex - startIndex

		return nil
	})

	if (tErr != nil) {
		return 0, tErr
	}

	return dayIndex, nil
}

func getEcoDevicesDataRoutine(device *Device, wg *sync.WaitGroup) {
	defer wg.Done()

	for {
		ecoDevicesResponse, ecoDevicesErr := getEcoDevicesData(device)
		if (ecoDevicesErr != nil) {
			log.Fatal(ecoDevicesErr);
		}

		fmt.Println(device.Id);

		inputs := []EcoDevicesInput{
			{
				Id: "t1",
				Value: ecoDevicesResponse.T1Base,
			},
			{
				Id: "t2",
				Value: ecoDevicesResponse.T2Base,
			},
		}

		err := saveIndexes(device, inputs);
		if (err != nil) {
			log.Fatal(err)
		}

		data, err := buildMQTTData(device, ecoDevicesResponse)
		if (err != nil) {
			log.Fatal(err)
		}

		err = sendMQTTData(device, data)
		if (err != nil) {
			log.Fatal(err)
		}

		fmt.Println(data)

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

func buildMQTTData(device *Device, ecoDevicesData *EcoDevicesResponse) (*MQTTData, error) {
	startOfDayDate := time.Date(time.Now().Year(), time.Now().Month(), time.Now().Day(), 0, 0, 0, 0, time.Now().Location())
	startOfMonthDate := time.Date(time.Now().Year(), time.Now().Month(), 1, 0, 0, 0, 0, time.Now().Location())
	startOfYearDate := time.Date(time.Now().Year(), 1, 1, 0, 0, 0, 0, time.Now().Location())

	dayIndexT1, err := getIndexFromStartDate(device, "t1", &startOfDayDate)
	if (err != nil) {
		return nil, err
	}

	monthIndexT1, err := getIndexFromStartDate(device, "t1", &startOfMonthDate)
	if (err != nil) {
		return nil, err
	}

	yearIndexT1, err := getIndexFromStartDate(device, "t1", &startOfYearDate)
	if (err != nil) {
		return nil, err
	}

	return &MQTTData{
		T1Index: ecoDevicesData.T1Base,
		T1IndexDay: dayIndexT1,
		T1IndexMonth: monthIndexT1,
		T1IndexYear: yearIndexT1,
		T1Power: ecoDevicesData.T1Papp,
	}, nil
}

func sendMQTTData(device *Device, data *MQTTData) (error) {
	// Create an MQTT Client.
	cli := client.New(&client.Options{
		// Define the processing of the error handler.
		ErrorHandler: func(err error) {
			fmt.Println(err)
		},
	})

	// Terminate the Client.
	defer cli.Terminate()

	// Connect to the MQTT Server.
	err := cli.Connect(&client.ConnectOptions{
		Network:  "tcp",
		Address:  viper.GetString("mqtt.uri"),
		ClientID: []byte("eco-devices-2-mqtt"),
	})

	if err != nil {
		return err
	}

	// Publish a message.
	err = cli.Publish(&client.PublishOptions{
		QoS:       mqtt.QoS0,
		TopicName: []byte(viper.GetString("mqtt.topic-level") + "/" + device.Id + "/" + viper.GetString("electricity.topic-level") + "/1/index"),
		Message:   []byte(strconv.Itoa(data.T1Index)),
	})
	if err != nil {
		return err
	}

	return nil
}

func main() {
	var wg sync.WaitGroup

	initConfig()
	initDevices(&wg)

	wg.Wait()
}
