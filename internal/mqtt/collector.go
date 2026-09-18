// Package mqtt collects power telemetry from the Tasmota wattmeter via MQTT.
package mqtt

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
)

// Config are the broker connection parameters, read from the environment by the caller.
type Config struct {
	BrokerURL string // e.g. tcp://mqtt.example.lan:1883
	Username  string
	Password  string
	Topic     string // e.g. tele/tasmota/SENSOR
}

// Collector subscribes to the Tasmota topic and writes each sample to power_samples.
type Collector struct {
	db     *sql.DB
	client paho.Client
	topic  string
}

// New creates the collector. The library handles auto-reconnect: connection
// state is only ever logged, never a fatal error.
func New(db *sql.DB, cfg Config) *Collector {
	c := &Collector{db: db, topic: cfg.Topic}

	opts := paho.NewClientOptions().
		AddBroker(cfg.BrokerURL).
		SetClientID("wattson-collector").
		SetUsername(cfg.Username).
		SetPassword(cfg.Password).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(5 * time.Second).
		SetOnConnectHandler(func(paho.Client) {
			log.Println("mqtt: connected, subscribing to", cfg.Topic)
		}).
		SetConnectionLostHandler(func(_ paho.Client, err error) {
			log.Println("mqtt: connection lost, retrying:", err)
		})
	opts.OnConnect = func(cl paho.Client) {
		if tok := cl.Subscribe(cfg.Topic, 0, c.handleMessage); tok.Wait() && tok.Error() != nil {
			log.Println("mqtt: subscribe failed:", tok.Error())
		}
	}

	c.client = paho.NewClient(opts)
	return c
}

// Start connects without blocking the caller: with SetConnectRetry on, the
// first attempt (and later retries) run inside the MQTT client in the
// background. Waiting here for that first attempt would block the whole HTTP
// server's startup until the broker responds or times out — unacceptable if
// the broker is briefly down. Connect/reconnect outcomes are already logged
// by the OnConnect/OnConnectionLost handlers.
func (c *Collector) Start() error {
	c.client.Connect()
	return nil
}

func (c *Collector) Stop() {
	c.client.Disconnect(250)
}

func (c *Collector) handleMessage(_ paho.Client, msg paho.Message) {
	watts, kwh, voltage, current, err := parseLine1(msg.Payload())
	if err != nil {
		log.Println("mqtt: invalid payload, dropped:", err)
		return
	}
	if err := insertSample(c.db, time.Now().Unix(), watts, kwh, voltage, current); err != nil {
		log.Println("mqtt: writing sample failed:", err)
	}
}

// tasmotaSensor mirrors the payload published on tele/<topic>/SENSOR. The
// Power/Total/Current arrays have one element per line; only index 0 is used.
type tasmotaSensor struct {
	ENERGY struct {
		Total   []float64 `json:"Total"`
		Power   []float64 `json:"Power"`
		Voltage float64   `json:"Voltage"`
		Current []float64 `json:"Current"`
	} `json:"ENERGY"`
}

func parseLine1(payload []byte) (watts, cumulativeKwh, voltage, current float64, err error) {
	var s tasmotaSensor
	if err := json.Unmarshal(payload, &s); err != nil {
		return 0, 0, 0, 0, fmt.Errorf("json: %w", err)
	}
	if len(s.ENERGY.Power) == 0 || len(s.ENERGY.Total) == 0 || len(s.ENERGY.Current) == 0 {
		return 0, 0, 0, 0, fmt.Errorf("missing or empty ENERGY fields: %s", payload)
	}
	return s.ENERGY.Power[0], s.ENERGY.Total[0], s.ENERGY.Voltage, s.ENERGY.Current[0], nil
}

func insertSample(db *sql.DB, ts int64, watts, kwh, voltage, current float64) error {
	_, err := db.Exec(
		`INSERT OR REPLACE INTO power_samples (ts, watts, cumulative_kwh, voltage, current) VALUES (?, ?, ?, ?, ?)`,
		ts, watts, kwh, voltage, current,
	)
	return err
}
