package main

import (
	"context"
	"flag"
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

type Tenancy struct {
	Name          string `yaml:"name"`
	TenancyID     string `yaml:"tenancy_id"`
	CompartmentID string `yaml:"compartment_id"`
	Region        string `yaml:"region"`
}

type TenancyConfig struct {
	Tenancies []Tenancy `yaml:"tenancies"`
}

type MetricNamespace struct {
	Namespace     string   `yaml:"namespace"`
	Names         []string `yaml:"names"`
	ResourceGroup string   `yaml:"resource_group,omitempty"`
	Resolution    string   `yaml:"resolution,omitempty"`
}

type MetricConfig struct {
	Metrics []MetricNamespace `yaml:"metrics"`
}

var ociMetric = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "oci_metric_value",
		Help: "OCI Monitoring metric value",
	},
	[]string{"tenancy", "region", "namespace", "metric", "dimension_key", "dimension_value"},
)

func init() {
	prometheus.MustRegister(ociMetric)
}

func loadConfigs() (TenancyConfig, MetricConfig) {
	var tenants TenancyConfig
	var metrics MetricConfig

	tenantsFile, err := ioutil.ReadFile("config/tenants.yaml")
	if err != nil {
		log.Fatalf("Cannot read tenants.yaml: %v", err)
	}
	if err := yaml.Unmarshal(tenantsFile, &tenants); err != nil {
		log.Fatalf("Invalid tenants.yaml: %v", err)
	}

	metricsFile, err := ioutil.ReadFile("config/metrics.yaml")
	if err != nil {
		log.Fatalf("Cannot read metrics.yaml: %v", err)
	}
	if err := yaml.Unmarshal(metricsFile, &metrics); err != nil {
		log.Fatalf("Invalid metrics.yaml: %v", err)
	}

	return tenants, metrics
}

func collectMetrics(provider common.ConfigurationProvider, tenants TenancyConfig, metrics MetricConfig) {
	for _, tenant := range tenants.Tenancies {
		client, err := monitoring.NewMonitoringClientWithConfigurationProvider(provider)
		if err != nil {
			log.Printf("Error creating monitoring client for %s: %v", tenant.Name, err)
			continue
		}
		client.SetRegion(tenant.Region)

		current := time.Now().UTC().Add(-5 * time.Minute)
		endTime := time.Now().UTC()

		start := common.SDKTime{Time: current}
		end := common.SDKTime{Time: endTime}

		for _, m := range metrics.Metrics {
			for _, metricName := range m.Names {
				query := fmt.Sprintf("%s[1m].mean()", metricName)

				req := monitoring.SummarizeMetricsDataRequest{
					CompartmentId:          common.String(tenant.CompartmentID),
					CompartmentIdInSubtree: common.Bool(true),
					SummarizeMetricsDataDetails: monitoring.SummarizeMetricsDataDetails{
						Namespace:     common.String(m.Namespace),
						Query:         common.String(query),
						StartTime:     &start,
						EndTime:       &end,
					},
				}

				// Optional fields
				if m.ResourceGroup != "" {
					req.SummarizeMetricsDataDetails.ResourceGroup = common.String(m.ResourceGroup)
				}
				if m.Resolution != "" {
					req.SummarizeMetricsDataDetails.Resolution = common.String(m.Resolution)
				}

				response, err := client.SummarizeMetricsData(context.Background(), req)
				if err != nil {
					log.Printf("Failed to get %s from %s: %v", metricName, tenant.Name, err)
					continue
				}

				for _, item := range response.Items {
					if len(item.AggregatedDatapoints) == 0 {
						continue
					}
					latest := item.AggregatedDatapoints[len(item.AggregatedDatapoints)-1]

					dimKey, dimValue := "resourceId", item.Dimensions["resourceId"]
					if dimValue == "" {
						// fallback
						for k, v := range item.Dimensions {
							if v != "" {
								dimKey, dimValue = k, v
								break
							}
						}
					}

					ociMetric.With(prometheus.Labels{
						"tenancy":         tenant.Name,
						"region":          tenant.Region,
						"namespace":       m.Namespace,
						"metric":          metricName,
						"dimension_key":   dimKey,
						"dimension_value": dimValue,
					}).Set(*latest.Value)
				}
			}
		}
	}
}

func main() {
	configPath := flag.String("config", "", "Path to OCI API config file")
	listen := flag.String("listen-address", ":8080", "Exporter listen address")
	flag.Parse()

	if *configPath == "" {
		fmt.Println("Missing required flag: -config")
		os.Exit(1)
	}

	provider, err := common.ConfigurationProviderFromFile(*configPath, "")
	if err != nil {
		log.Fatalf("Failed to load OCI config: %v", err)
	}

	tenants, metrics := loadConfigs()

	go func() {
		for {
			collectMetrics(provider, tenants, metrics)
			time.Sleep(60 * time.Second)
		}
	}()

	http.Handle("/metrics", promhttp.Handler())
	log.Printf("Exporter running on %s", *listen)
	log.Fatal(http.ListenAndServe(*listen, nil))
}
