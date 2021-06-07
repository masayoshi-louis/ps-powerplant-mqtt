package main

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/antchfx/xmlquery"
	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/google/uuid"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
)

var config struct {
	Device struct {
		Name         string `yaml:"Name"`
		Host         string `yaml:"Host"`
		Port         int64  `yaml:"Port"`
		Zones        int    `yaml:"Zones"`
		PollInterval string `yaml:"PollInterval"`
		Username     string `yaml:"Username"`
		Password     string `yaml:"Password"`
	} `yaml:"Device"`
	MQTT struct {
		Host     string `yaml:"Host"`
		Port     int64  `yaml:"Port"`
		Username string `yaml:"Username"`
		Password string `yaml:"Password"`
	} `yaml:"MQTT"`
}

var mqttClient mqtt.Client

func main() {
	initConfig()
	pollInterval, err := time.ParseDuration(config.Device.PollInterval)
	if err != nil {
		logrus.WithError(err).Error("config error")
		panic(err)
	}

	initMQTT()
	mqttSubscribe()

	for {
		logrus.Info("push state begin")
		err := pushState()
		if err != nil {
			logrus.WithError(err).Error("push state error")
		}
		logrus.Info("push state end")
		time.Sleep(pollInterval)
	}
}

func initConfig() {
	logrus.Info("config load begin")
	data, err := ioutil.ReadFile("./config.yml")
	if err != nil {
		logrus.WithError(err).Error("config read error")
		panic(err)
	}
	err = yaml.Unmarshal(data, &config)
	if err != nil {
		logrus.WithError(err).Error("config load error")
		panic(err)
	}
	logrus.Info("config load done")
}

func initMQTT() {
	logrus.Info("mqtt connect begin")
	opts := mqtt.NewClientOptions()
	opts.AddBroker(fmt.Sprintf("tcp://%s:%d", config.MQTT.Host, config.MQTT.Port))
	opts.SetClientID(fmt.Sprintf("ps-pp-%s", uuid.New().String()))
	opts.SetUsername(config.MQTT.Username)
	opts.SetPassword(config.MQTT.Password)
	mqttClient = mqtt.NewClient(opts)
	connectToken := mqttClient.Connect()
	connectToken.Wait()
	err := connectToken.Error()
	if err != nil {
		logrus.WithError(err).Error("mqtt connect error")
		panic(err)
	}
	logrus.Info("mqtt connect done")
}

func mqttSubscribe() {
	buildZone := func(topic string) *zone {
		topicSlice := strings.Split(topic, "/")
		z, _ := strconv.ParseInt(topicSlice[4], 10, 0)
		return &zone{
			Id: z,
			On: -1,
		}
	}
	mqttClient.Subscribe(fmt.Sprintf("ps-audio/power-plant/%s/zones/+/power/set", config.Device.Name), 1, func(client mqtt.Client, message mqtt.Message) {
		logrus.Infof("message receive topic:%v payload:%v", message.Topic(), message.Payload())
		z := buildZone(message.Topic())
		mode := strings.ToLower(string(message.Payload()))
		if mode == "off" {
			z.On = 0
		} else {
			z.On = 1
		}
		err := setState(z)
		if err != nil {
			logrus.WithError(err).Errorf("set mode state error u:%+v", z)
		}
		message.Ack()
	})
}

func setState(z *zone) error {
	list, err := listZones()
	if err != nil {
		return errors.Wrap(err, "list zone error")
	}
	cz := list[z.Id]
	if cz.On == z.On {
		return nil
	}
	u := fmt.Sprintf("http://%s:%d/zones.cgi", config.Device.Host, config.Device.Port)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return errors.Wrap(err, "create request error")
	}
	queries := url.Values{}
	queries.Add("zone", strconv.FormatInt(z.Id, 10))
	req.URL.RawQuery = queries.Encode()
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return errors.Wrap(err, "invoke request error")
	}
	if resp.StatusCode != 200 {
		return errors.Errorf("request error, http status: %s", resp.Status)
	}
	return nil
}

type zone struct {
	Id int64 `json:"id"`
	On int64 `json:"on"`
}

func pushState() error {
	list, err := listZones()
	if err != nil {
		return errors.Wrap(err, "list zone error")
	}
	for _, z := range list {
		logrus.Infof("mqtt publish begin, zone id:%d", z.Id)
		powerState := "OFF"
		if z.On == 1 {
			powerState = "ON"
		} else if z.On < 0 {
			continue
		}
		err = mqttPublish(fmt.Sprintf("ps-audio/power-plant/%s/zones/%d/power/state", config.Device.Name, z.Id), powerState)
		if err != nil {
			logrus.WithError(err).Errorf("publish mode state off error zone:%+v", z)
		}
	}
	return nil
}

func listZones() ([]*zone, error) {
	u := fmt.Sprintf("http://%s:%d/status.xml", config.Device.Host, config.Device.Port)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, errors.Wrap(err, "create request error")
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return nil, errors.Wrap(err, "invoke request error")
	}
	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, errors.Wrap(err, "read response body error")
	}
	var zones []*zone
	doc, err := xmlquery.Parse(bytes.NewReader(respBody))
	xmlquery.FindEach(doc, "//power", func(i int, node *xmlquery.Node) {
		on, err := strconv.ParseInt(node.FirstChild.Data, 10, 0)
		if err != nil {
			on = -1
		}
		zones = append(zones, &zone{
			Id: 0,
			On: on,
		})
	})
	xmlquery.FindEach(doc, "//*[starts-with(local-name(), 'zone')]", func(i int, node *xmlquery.Node) {
		if i >= config.Device.Zones {
			return
		}
		on, err := strconv.ParseInt(node.FirstChild.Data, 10, 0)
		if err != nil {
			on = -1
		}
		zones = append(zones, &zone{
			Id: int64(i) + 1,
			On: on,
		})
	})
	return zones, nil
}

func mqttPublish(topic, payload string) error {
	publishToken := mqttClient.Publish(topic, 1, false, payload)
	publishToken.Wait()
	err := publishToken.Error()
	if err != nil {
		return errors.Wrap(err, "mqtt publish error")
	}
	return nil
}
