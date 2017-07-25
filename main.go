package main

import (
	"github.com/spf13/viper"
	"github.com/spf13/cast"
	"time"
	"net/http"
	"io/ioutil"
	"encoding/json"
	"github.com/boltdb/bolt"
	"errors"
	"strconv"
	MQTT "github.com/eclipse/paho.mqtt.golang"
	"regexp"
	"github.com/spf13/cobra"
	"github.com/op/go-logging"
)

const version = "0.1.0"

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

var RootCmd = &cobra.Command{
	Use:   "go-ecodevices-mqtt",
	Short: "Bind your GCE Ecodevices to MQTT",
	Long: `Bind your GCE Ecodevices to MQTT`,
	Run: func(cmd *cobra.Command, args []string) {
		initConfig()
		device, err := getEcoDevices()
		if (err != nil) {
			log.Fatal(err)
		}
		log.Debugf("new ecodevices added : %s", device)
		deviceMeasures, err := getEcoDevicesMeasures()
		if (err != nil) {
			log.Fatal(err)
		}
		log.Debugf("defined measures for %s : %s", device.Id, deviceMeasures)
		getEcoDevicesDataRoutine(device, deviceMeasures)
	},
}

var (
	cfgFile string
	log        *logging.Logger
)

func init() {
	RootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file")

	validConfigFilenames := []string{"json", "js", "yaml", "yml", "toml", "tml"}
	_ = RootCmd.PersistentFlags().SetAnnotation("config", cobra.BashCompFilenameExt, validConfigFilenames)
}

func main() {
	if err := RootCmd.Execute(); err != nil {
		log.Fatal(err)
	}
}

func initConfig() () {
	if (cfgFile) == "" {
		log.Fatal("please specify a config file")
	}

	viper.SetConfigFile(cfgFile)

	err := viper.ReadInConfig()
	if err != nil {
		log.Fatal(err)
	}

	if (viper.GetString("db.path") == "") {
		log.Fatal("please specify a db path in config")
	}

	log = logging.MustGetLogger("ecodevices-mqtt")
	format := logging.MustStringFormatter(
		`%{color}%{time:2006-01-02T15:04:05.999Z-07:00} [%{level}] %{color:reset}%{message}`,
	)
	logging.SetFormatter(format)
	switch viper.GetString("log.level") {
	case "CRITICAL":
		logging.SetLevel(logging.CRITICAL, "")
		break
	case "ERROR":
		logging.SetLevel(logging.ERROR, "")
		break
	case "WARNING":
		logging.SetLevel(logging.WARNING, "")
		break
	case "NOTICE":
		logging.SetLevel(logging.NOTICE, "")
		break
	case "INFO":
		logging.SetLevel(logging.INFO, "")
		break
	case "DEBUG":
		logging.SetLevel(logging.DEBUG, "")
		break
	default:
		logging.SetLevel(logging.NOTICE, "")
	}
}

func getEcoDevicesData(device *EcoDevices) (*EcoDevicesResponse, error) {
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

func saveIndex(device *EcoDevices, jsonKey string, index int) (error) {
	db, err := bolt.Open(viper.GetString("db.path"), 0600, nil)
	if err != nil {
		return err
	}
	defer db.Close()

	log.Debugf("saving current index (%d) in database for %s", index, device.Id)
	db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(device.Id + "-" + jsonKey + "-indexes"))
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

func saveIndexes(device *EcoDevices, measures []EcoDevicesMeasure, ecoDevicesResponse *EcoDevicesResponse) (error) {
	for _, measure := range measures {
		if (measure.Index) {
			value, err := getEcoDevicesValueFromJSONKey(measure.JsonKey, ecoDevicesResponse)
			if (err != nil) {
				return err;
			}

			if (value != 0) {
				err := saveIndex(device, measure.JsonKey, value)
				if (err != nil) {
					return err
				}
			}
		}
	}

	return nil
}

func getIndexFromStartDate(device *EcoDevices, jsonKey string, startDate *time.Time) (int, error) {
	var dayIndex int
	bucketName := device.Id + "-" + jsonKey + "-indexes"

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

func getEcoDevicesDataRoutine(device *EcoDevices, measures []EcoDevicesMeasure) {
	for {
		ecoDevicesResponse, err := getEcoDevicesData(device)
		if (err != nil) {
			log.Error(err)
		}
		log.Debugf("json response for %s : %s", device.Id, ecoDevicesResponse)

		err = saveIndexes(device, measures, ecoDevicesResponse);
		if (err != nil) {
			log.Error(err)
		}

		err = publishOnMQTT(device, measures, ecoDevicesResponse)
		if (err != nil) {
			log.Error(err)
		}

		time.Sleep(time.Duration(device.CheckEvery) * time.Second)
	}
}

func getEcoDevicesValueFromJSONKey(jsonKey string, ecoDevicesResponse *EcoDevicesResponse) (int, error) {
	switch jsonKey {
	case "T1_BASE":
		return ecoDevicesResponse.T1Base, nil
	case "T2_BASE":
		return ecoDevicesResponse.T2Base, nil
	case "T1_PAPP":
		return ecoDevicesResponse.T1Papp, nil
	case "T2_PAPP":
		return ecoDevicesResponse.T2Papp, nil
	case "INDEX_C1":
		return ecoDevicesResponse.C1Index, nil
	case "INDEX_C2":
		return ecoDevicesResponse.C2Index, nil
	default:
		return 0, errors.New("unrecognized json key : " + jsonKey)
	}
}

func getPriceFromMeasure(measure EcoDevicesMeasure) (float64, error) {
	if (measure.PriceURL != "") {
		electricityPriceClient := http.Client{
			Timeout: time.Second * 5,
		}

		req, err := http.NewRequest(http.MethodGet, measure.PriceURL, nil)
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
		return measure.Price, nil
	}

	return 0.0, nil
}

func getEcoDevicesMeasures() ([]EcoDevicesMeasure, error) {
	configMeasures, err := viper.Get("eco-devices-measures").([]interface{})

	if (!err) {
		return nil, errors.New("please specify at least one eco-devices measure in config");
	}
	measures := make([]EcoDevicesMeasure, len(configMeasures))
	for index, table := range configMeasures {
		if measure, err := table.(map[string]interface{}); err {
			if (!allowedJSONKeys[cast.ToString(measure["json-key"])]) {
				return nil, errors.New("please specify a valid json-key for eco-devices measure in config")
			}

			if (cast.ToString(measure["mqtt-topic-level"]) == "") {
				return nil, errors.New("please specify an mqtt-topic-level for eco-devices measure in config");
			}

			measures[index] = EcoDevicesMeasure{
				JsonKey: cast.ToString(measure["json-key"]),
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
	if (!viper.IsSet("eco-devices")) {
		return nil, errors.New("Please define an eco-devices in config")
	}

	if (!viper.IsSet("eco-devices.ip")) {
		return nil, errors.New("Please define an ip for eco-devices in config")
	}

	id := "eco-devices-0"
	if (viper.IsSet("eco-devices.id")) {
		id = viper.GetString("eco-devices.id")
	}

	checkEvery := 60
	if (viper.IsSet("eco-devices.check-every")) {
		checkEvery = viper.GetInt("eco-devices.check-every")
	}

	return &EcoDevices{
		Id: id,
		Ip: viper.GetString("eco-devices.ip"),
		CheckEvery: checkEvery,
	}, nil
}

func publishOnMQTT(device *EcoDevices, measures []EcoDevicesMeasure, ecoDevicesResponse *EcoDevicesResponse) (error) {
	startOfDayDate := time.Date(time.Now().Year(), time.Now().Month(), time.Now().Day(), 0, 0, 0, 0, time.Now().Location())
	startOfMonthDate := time.Date(time.Now().Year(), time.Now().Month(), 1, 0, 0, 0, 0, time.Now().Location())
	startOfYearDate := time.Date(time.Now().Year(), 1, 1, 0, 0, 0, 0, time.Now().Location())

	opts := MQTT.NewClientOptions()
	opts.AddBroker(viper.GetString("mqtt.uri"))
	opts.SetClientID(viper.GetString("mqtt.client-id"))

	c := MQTT.NewClient(opts)
	if token := c.Connect(); token.WaitTimeout(time.Second * 5) && token.Error() != nil {
		return token.Error()
	}

	for _, measure := range measures {
		topic := viper.GetString("mqtt.topic-level") + "/" + device.Id + "/" + measure.MQTTTopicLevel

		ecoDeviceValue, _ := getEcoDevicesValueFromJSONKey(measure.JsonKey, ecoDevicesResponse)
		if (ecoDeviceValue != 0) {
			log.Debugf("publish %s on %s", strconv.Itoa(ecoDeviceValue), topic)
			token := c.Publish(topic, byte(measure.MQTTQoS), measure.MQTTRetained, strconv.Itoa(ecoDeviceValue))
			token.WaitTimeout(time.Second * 5)
		}

		currentPrice, errPrice := getPriceFromMeasure(measure)
		if (errPrice != nil) {
			log.Error(errPrice)
		}

		if (currentPrice > 0.0) {
			log.Debugf("publish %s on %s", strconv.FormatFloat(currentPrice, 'f', 3, 64), topic + "/price")
			token := c.Publish(topic + "/price", byte(measure.MQTTQoS), measure.MQTTRetained, strconv.FormatFloat(currentPrice, 'f', 3, 64))
			token.WaitTimeout(time.Second * 5)
		}

		if (measure.Index) {
			savedValue, _ := getIndexFromStartDate(device, measure.JsonKey, &startOfDayDate)
			log.Debugf("day index for %s is %d", device.Id, savedValue)
			if (savedValue != 0) {
				log.Debugf("publish %s on %s", strconv.Itoa(savedValue), topic + "/day")
				token := c.Publish(topic + "/day", byte(measure.MQTTQoS), measure.MQTTRetained, strconv.Itoa(savedValue))
				token.WaitTimeout(time.Second * 5)

				if (currentPrice > 0.0) {
					log.Debugf("publish %s on %s", strconv.FormatFloat((float64(savedValue) / float64(1000)) * currentPrice, 'f', 2, 64), topic + "/price/day")
					token := c.Publish(topic + "/price/day", byte(measure.MQTTQoS), measure.MQTTRetained, strconv.FormatFloat((float64(savedValue) / float64(1000)) * currentPrice, 'f', 2, 64))
					token.WaitTimeout(time.Second * 5)
				}
			}

			savedValue, _ = getIndexFromStartDate(device, measure.JsonKey, &startOfMonthDate)
			log.Debugf("month index for %s is %d", device.Id, savedValue)
			if (savedValue != 0) {
				log.Debugf("publish %s on %s", strconv.Itoa(savedValue), topic + "/month")
				token := c.Publish(topic + "/month", byte(measure.MQTTQoS), measure.MQTTRetained, strconv.Itoa(savedValue))
				token.WaitTimeout(time.Second * 5)

				if (currentPrice > 0.0) {
					log.Debugf("publish %s on %s", strconv.FormatFloat((float64(savedValue) / float64(1000)) * currentPrice, 'f', 2, 64), topic + "/price/month")
					token := c.Publish(topic + "/price/month", byte(measure.MQTTQoS), measure.MQTTRetained, strconv.FormatFloat((float64(savedValue) / float64(1000)) * currentPrice, 'f', 2, 64))
					token.WaitTimeout(time.Second * 5)
				}
			}

			savedValue, _ = getIndexFromStartDate(device, measure.JsonKey, &startOfYearDate)
			log.Debugf("year index for %s is %d", device.Id, savedValue)
			if (savedValue != 0) {
				log.Debugf("publish %s on %s", strconv.Itoa(savedValue), topic + "/year")
				token := c.Publish(topic + "/year", byte(measure.MQTTQoS), measure.MQTTRetained, strconv.Itoa(savedValue))
				token.WaitTimeout(time.Second * 5)

				if (currentPrice > 0.0) {
					log.Debugf("publish %s on %s", strconv.FormatFloat((float64(savedValue) / float64(1000)) * currentPrice, 'f', 2, 64), topic + "/price/year")
					token := c.Publish(topic + "/price/year", byte(measure.MQTTQoS), measure.MQTTRetained, strconv.FormatFloat((float64(savedValue) / float64(1000)) * currentPrice, 'f', 2, 64))
					token.WaitTimeout(time.Second * 5)
				}
			}
		}
	}

	c.Disconnect(250)
	return nil
}
