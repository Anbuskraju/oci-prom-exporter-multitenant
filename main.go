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
	Namespace string   `yaml:"namespace"`
	Names     []string `yaml:"names"`
}

type MetricConfig struct {
	Metrics []MetricNamespace `yaml:"metrics"`
}

var ociMetric = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "oci_metric_value",
		Help: "OCI Monitoring metric value",
	},
	[]string{"tenancy", "region", "namespace", "metric", "resource", "name"},
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

		start := time.Now().UTC().Add(-5 * time.Minute)
		end := time.Now().UTC()

		for _, m := range metrics.Metrics {
			for _, name := range m.Names {
				query := fmt.Sprintf("%s[1m].mean{}", name)

				request := monitoring.SummarizeMetricsDataRequest{
					CompartmentId:          common.String(tenant.CompartmentID),
					CompartmentIdInSubtree: common.Bool(true),
					SummarizeMetricsDataDetails: monitoring.SummarizeMetricsDataDetails{
						Namespace: common.String(m.Namespace),
						Query:     common.String(query),
						StartTime: &start,
						EndTime:   &end,
					},
				}

				response, err := client.SummarizeMetricsData(context.Background(), request)
				if err != nil {
					log.Printf("Failed to get metric %s from %s: %v", name, tenant.Name, err)
					continue
				}

				for _, item := range response.Items {
					for _, dp := range item.AggregatedDatapoints {
						ociMetric.With(prometheus.Labels{
							"tenancy":   tenant.Name,
							"region":    tenant.Region,
							"namespace": m.Namespace,
							"metric":    name,
							"resource":  item.Dimensions["resourceId"],
							"name":      item.Dimensions["resourceDisplayName"],
						}).Set(*dp.Value)
					}
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
