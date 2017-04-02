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
	MQTT "github.com/eclipse/paho.mqtt.golang"
	"regexp"
)

type Device struct {
	Id         string
	Ip         string
	CheckEvery int
}

type EcoDevicesInput struct {
	Id             string
	MQTTTopicLevel string
	MQTTQoS        int
	MQTTRetained   bool
	PriceURL       string
	Price          float64
}

type EcoDevicesResponse struct {
	T1Base  int `json:"T1_BASE"`
	T1Papp  int `json:"T1_PAPP"`
	T2Base  int `json:"T2_BASE"`
	T2Papp  int `json:"T2_PAPP"`
	C1Index int `json:"INDEX_C1"`
	C2Index int `json:"INDEX_C2"`
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

func saveIndexes(device *Device, inputs []EcoDevicesInput, ecoDevicesResponse *EcoDevicesResponse) (error) {
	for _, input := range inputs {
		value, err := getEcoDevicesIndexFromInput(input, ecoDevicesResponse)
		if (err != nil) {
			return err;
		}

		if (value != 0) {
			err := saveIndex(device, input.Id, value)
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

func getEcoDevicesDataRoutine(device *Device, inputs []EcoDevicesInput, wg *sync.WaitGroup) {
	defer wg.Done()

	for {
		ecoDevicesResponse, err := getEcoDevicesData(device)
		if (err != nil) {
			fmt.Println(err)
		}

		err = saveIndexes(device, inputs, ecoDevicesResponse);
		if (err != nil) {
			fmt.Println(err)
		}

		err = publishOnMQTT(device, inputs, ecoDevicesResponse)
		if (err != nil) {
			fmt.Println(err)
		}

		time.Sleep(time.Duration(device.CheckEvery) * time.Second)
	}
}

func getEcoDevicesIndexFromInput(input EcoDevicesInput, ecoDevicesResponse *EcoDevicesResponse) (int, error) {
	switch input.Id {
	case "T1":
		return ecoDevicesResponse.T1Base, nil
	case "T2":
		return ecoDevicesResponse.T2Base, nil
	case "C1":
		return ecoDevicesResponse.C1Index, nil
	case "C2":
		return ecoDevicesResponse.C2Index, nil
	default:
		return 0, errors.New("unrecognized input id")
	}
}

func getPriceFromInput(input EcoDevicesInput) (float64, error) {
	if (input.PriceURL != "") {
		electricityPriceClient := http.Client{
			Timeout: time.Second * 5,
		}

		req, err := http.NewRequest(http.MethodGet, input.PriceURL, nil)
		if err != nil {
			return 0.0, err
		}

		res, getErr := electricityPriceClient.Do(req)
		if getErr != nil {
			return 0.0, getErr
		}

		body, readErr := ioutil.ReadAll(res.Body)
		if readErr != nil {
			return 0.0, readErr
		}

		var re = regexp.MustCompile(`\d+\.\d*`)

		for _, match := range re.FindAllString(string(body), -1) {
			price, err := strconv.ParseFloat(match, 64)
			if err != nil {
				return 0.0, err
			}
			return price, nil
		}
	} else {
		return input.Price, nil
	}

	return 0.0, nil
}

func getInputs() ([]EcoDevicesInput) {
	inputs, err := viper.Get("inputs").([]interface{})

	if (!err) {
		log.Fatal("please specify at least one input in config");
	} else {
		retInputs := make([]EcoDevicesInput, len(inputs))
		for index, table := range inputs {
			if input, err := table.(map[string]interface{}); err {
				if (cast.ToString(input["id"]) != "T1" && cast.ToString(input["id"]) != "T2" && cast.ToString(input["id"]) != "C1" && cast.ToString(input["id"]) != "C2") {
					log.Fatal("please specify a valid id (T1, T2, C1 or C2) for input in config");
				}
				if (cast.ToString(input["mqtt-topic-level"]) == "") {
					log.Fatal("please specify an mqtt-topic-level for '" + cast.ToString(input["id"]) + "'input in config");
				}
				retInputs[index] = EcoDevicesInput{
					Id: cast.ToString(input["id"]),
					MQTTTopicLevel: cast.ToString(input["mqtt-topic-level"]),
					MQTTQoS: cast.ToInt(input["mqtt-qos"]),
					MQTTRetained: cast.ToBool(input["mqtt-retained"]),
					Price: cast.ToFloat64(input["price"]),
					PriceURL: cast.ToString(input["price-url"]),
				}
			}
		}
		return retInputs
	}
	return nil
}

func initDevices(inputs []EcoDevicesInput, wg *sync.WaitGroup) {
	devices, devicesErr := viper.Get("eco-devices").([]interface{})
	if !devicesErr {
		log.Fatal("please specify at least one eco-devices in config");
	} else {
		wg.Add(len(devices))
		for index, table := range devices {
			if device, deviceErr := table.(map[string]interface{}); deviceErr {
				if (cast.ToString(device["id"]) == "") {
					device["id"] = "eco-devices-" + cast.ToString(index)
				}

				if (cast.ToString(device["ip"]) == "") {
					log.Fatal("please specify an ip address for '" + cast.ToString(device["id"]) + "' device in config");
				}

				if (cast.ToInt(device["check-every"]) == 0) {
					device["check-every"] = 30
				}

				go getEcoDevicesDataRoutine(&Device{
					Id: cast.ToString(device["id"]),
					Ip: cast.ToString(device["ip"]),
					CheckEvery: cast.ToInt(device["check-every"]),
				}, inputs, wg)
			}
		}
	}
}

func publishOnMQTT(device *Device, inputs []EcoDevicesInput, ecoDevicesResponse *EcoDevicesResponse) (error) {
	startOfDayDate := time.Date(time.Now().Year(), time.Now().Month(), time.Now().Day(), 0, 0, 0, 0, time.Now().Location())
	startOfMonthDate := time.Date(time.Now().Year(), time.Now().Month(), 1, 0, 0, 0, 0, time.Now().Location())
	startOfYearDate := time.Date(time.Now().Year(), 1, 1, 0, 0, 0, 0, time.Now().Location())

	opts := MQTT.NewClientOptions()
	opts.AddBroker(viper.GetString("mqtt.uri"))
	opts.SetClientID(viper.GetString("mqtt.client-id"))

	c := MQTT.NewClient(opts)
	if token := c.Connect(); token.Wait() && token.Error() != nil {
		return token.Error()
	}

	for _, input := range inputs {
		topicPrepend := viper.GetString("mqtt.topic-level") + "/" + device.Id + "/" + input.MQTTTopicLevel + "/"

		currentIndex, _ := getEcoDevicesIndexFromInput(input, ecoDevicesResponse)
		if (currentIndex != 0) {
			token := c.Publish(topicPrepend + "index", byte(input.MQTTQoS), input.MQTTRetained, strconv.Itoa(currentIndex))
			token.Wait()
		}

		currentPrice, _ := getPriceFromInput(input)
		if (currentPrice > 0.0) {
			token := c.Publish(topicPrepend + "price", byte(input.MQTTQoS), input.MQTTRetained, strconv.FormatFloat(currentPrice, 'f', 3, 64))
			token.Wait()
		}

		dayIndex, _ := getIndexFromStartDate(device, input.Id, &startOfDayDate)
		if (dayIndex != 0) {
			token := c.Publish(topicPrepend + "index/day", byte(input.MQTTQoS), input.MQTTRetained, strconv.Itoa(dayIndex))
			token.Wait()

			if (currentPrice > 0.0) {
				token := c.Publish(topicPrepend + "price/day", byte(input.MQTTQoS), input.MQTTRetained, strconv.FormatFloat((float64(dayIndex) / float64(1000)) * currentPrice, 'f', 2, 64))
				token.Wait()
			}
		}

		monthIndex, _ := getIndexFromStartDate(device, input.Id, &startOfMonthDate)
		if (monthIndex != 0) {
			token := c.Publish(topicPrepend + "index/month", byte(input.MQTTQoS), input.MQTTRetained, strconv.Itoa(monthIndex))
			token.Wait()

			if (currentPrice > 0.0) {
				token := c.Publish(topicPrepend + "price/month", byte(input.MQTTQoS), input.MQTTRetained, strconv.FormatFloat((float64(monthIndex) / float64(1000)) * currentPrice, 'f', 2, 64))
				token.Wait()
			}
		}

		yearIndex, _ := getIndexFromStartDate(device, input.Id, &startOfYearDate)
		if (yearIndex != 0) {
			token := c.Publish(topicPrepend + "index/year", byte(input.MQTTQoS), input.MQTTRetained, strconv.Itoa(yearIndex))
			token.Wait()

			if (currentPrice > 0.0) {
				token := c.Publish(topicPrepend + "price/year", byte(input.MQTTQoS), input.MQTTRetained, strconv.FormatFloat((float64(yearIndex) / float64(1000)) * currentPrice, 'f', 2, 64))
				token.Wait()
			}
		}

	}

	c.Disconnect(250)
	return nil
}

func main() {
	var wg sync.WaitGroup

	initConfig()
	initDevices(getInputs(), &wg)

	wg.Wait()
}
