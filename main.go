package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/monitoring"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"gopkg.in/yaml.v3"
)

type TenancyConfig struct {
	Tenancies []struct {
		Name          string `yaml:"name"`
		TenancyID     string `yaml:"tenancy_id"`
		CompartmentID string `yaml:"compartment_id"`
		Region        string `yaml:"region"`
	} `yaml:"tenancies"`
}

type MetricConfig struct {
	Metrics []struct {
		Namespace string   `yaml:"namespace"`
		Names     []string `yaml:"names"`
	} `yaml:"metrics"`
}

var metricGauge = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "oci_metric_value",
		Help: "Generic OCI metric collector",
	},
	[]string{"tenancy", "region", "namespace", "metric", "resource", "name"},
)

func init() {
	prometheus.MustRegister(metricGauge)
}

func loadYAMLConfigs() (TenancyConfig, MetricConfig) {
	var tenants TenancyConfig
	var metrics MetricConfig

	tBytes, err := ioutil.ReadFile("config/tenants.yaml")
	if err != nil {
		log.Fatalf("Error reading tenants.yaml: %v", err)
	}
	err = yaml.Unmarshal(tBytes, &tenants)
	if err != nil {
		log.Fatalf("Error parsing tenants.yaml: %v", err)
	}

	mBytes, err := ioutil.ReadFile("config/metrics.yaml")
	if err != nil {
		log.Fatalf("Error reading metrics.yaml: %v", err)
	}
	err = yaml.Unmarshal(mBytes, &metrics)
	if err != nil {
		log.Fatalf("Error parsing metrics.yaml: %v", err)
	}

	return tenants, metrics
}

func collectMetrics() {
	tenants, metrics := loadYAMLConfigs()

	for _, tenant := range tenants.Tenancies {
		provider := common.NewRawConfigurationProvider(
			tenant.TenancyID,
			os.Getenv("OCI_USER_ID"),
			tenant.Region,
			os.Getenv("OCI_KEY_FINGERPRINT"),
			os.Getenv("OCI_PRIVATE_KEY_PATH"),
			nil,
		)

		client, err := monitoring.NewMonitoringClientWithConfigurationProvider(provider)
		if err != nil {
			log.Printf("Error creating monitoring client for %s: %v", tenant.Name, err)
			continue
		}

		now := time.Now().UTC()
		start := now.Add(-5 * time.Minute)

		for _, m := range metrics.Metrics {
			for _, name := range m.Names {
				query := fmt.Sprintf("%s[1m].mean{}", name)
				request := monitoring.SummarizeMetricsDataRequest{
					Namespace:                &m.Namespace,
					CompartmentId:            &tenant.CompartmentID,
					IsCompartmentIdInSubtree: common.Bool(true),
					Query:                    &query,
					StartTime:                &start,
					EndTime:                  &now,
				}

				resp, err := client.SummarizeMetricsData(context.Background(), request)
				if err != nil {
					log.Printf("Error fetching metrics from %s/%s: %v", m.Namespace, name, err)
					continue
				}

				for _, item := range resp.SummarizeMetricsDataResponseDetails {
					for _, point := range item.AggregatedDatapoints {
						resource := item.Dimensions["resourceId"]
						displayName := item.Dimensions["resourceDisplayName"]
						metricGauge.With(prometheus.Labels{
							"tenancy":   tenant.Name,
							"region":    tenant.Region,
							"namespace": m.Namespace,
							"metric":    name,
							"resource":  resource,
							"name":      displayName,
						}).Set(*point.Value)
					}
			}
		}
	}
	}
}

func main() {
	go func() {
		for {
			collectMetrics()
			time.Sleep(60 * time.Second)
		}
	}()

	http.Handle("/metrics", promhttp.Handler())
	log.Println("Listening on :2112")
	log.Fatal(http.ListenAndServe(":2112", nil))
}
