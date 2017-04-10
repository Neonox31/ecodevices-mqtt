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

type EcoDevices struct {
	Id         string
	Ip         string
	CheckEvery int
}

type EcoDevicesMeasure struct {
	JsonKey        string
	MQTTTopicLevel string
	MQTTQoS        int
	MQTTRetained   bool
	Index          bool
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

var allowedJSONKeys = map[string]bool{
	"T1_BASE": true,
	"T1_PAPP": true,
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

func getEcoDevicesMeasures() ([]EcoDevicesMeasure, error) {
	configMeasures, err := viper.Get("eco-devices-measures").([]interface{})

	if (err != nil) {
		return nil, errors.New("please specify at least one eco-devices measure in config");
	}
	measures := make([]EcoDevicesMeasure, len(configMeasures))
	for index, table := range measures {
		if measure, err := table.(map[string]interface{}); err {
			if (!allowedJSONKeys[cast.ToString(measure["json-key"])]) {
				return nil, errors.New("please specify a valid json-key for eco-devices measure in config")
			}

			if (cast.ToString(measure["mqtt-topic-level"]) == "") {
				return nil, errors.New("please specify an mqtt-topic-level for eco-devices measure in config");
			}

			measures[index] = EcoDevicesMeasure{
				MQTTTopicLevel: cast.ToString(measure["mqtt-topic-level"]),
				MQTTQoS: cast.ToInt(measure["mqtt-qos"]),
				MQTTRetained: cast.ToBool(measure["mqtt-retained"]),
				Index: cast.ToBool(measure["index"]),
				Price: cast.ToFloat64(measure["price"]),
				PriceURL: cast.ToString(measure["price-url"]),
			}
		}
	}
	return measures, nil
}

func getEcoDevices() (*EcoDevices, error) {
	if (!viper.IsSet("ecodevices")) {
		return nil, errors.New("Please define an eco-devices in config")
	}

	if (!viper.IsSet("ecodevices.ip")) {
		return nil, errors.New("Please define an ip for eco-devices in config")
	}

	id := "eco-devices-0"
	if (viper.IsSet("ecodevices.id")) {
		id = viper.GetString("ecodevices.id")
	}

	checkEvery := 60
	if (viper.IsSet("ecodevices.check-every")) {
		checkEvery = viper.GetInt("ecodevices.check-every")
	}

	return &EcoDevices{
		Id: id,
		Ip: viper.GetString("ecodevices.ip"),
		CheckEvery: checkEvery,
	}, nil
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
