package main

import (
    "context"
    "flag"
    "fmt"
    "io/ioutil"
    "log"
    "net/http"
    "os"
    "strings"
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

func loadConfigs() (TenancyConfig, MetricConfig) {
    var tenants TenancyConfig
    var metrics MetricConfig

    data, err := ioutil.ReadFile("config/tenants.yaml")
    if err != nil {
        log.Fatalf("Cannot read tenants.yaml: %v", err)
    }
    if err := yaml.Unmarshal(data, &tenants); err != nil {
        log.Fatalf("Invalid tenants.yaml: %v", err)
    }

    data, err = ioutil.ReadFile("config/metrics.yaml")
    if err != nil {
        log.Fatalf("Cannot read metrics.yaml: %v", err)
    }
    if err := yaml.Unmarshal(data, &metrics); err != nil {
        log.Fatalf("Invalid metrics.yaml: %v", err)
    }

    return tenants, metrics
}

func summarizeWithRetry(client monitoring.MonitoringClient, req monitoring.SummarizeMetricsDataRequest) (
    monitoring.SummarizeMetricsDataResponse, error,
) {
    var resp monitoring.SummarizeMetricsDataResponse
    var err error
    for i := 0; i < 3; i++ {
        resp, err = client.SummarizeMetricsData(context.Background(), req)
        if err == nil || !strings.Contains(err.Error(), "TooManyRequests") {
            return resp, err
        }
        backoff := time.Duration(1<<i) * time.Second
        log.Printf("TooManyRequests, backing off %v", backoff)
        time.Sleep(backoff)
    }
    return resp, err
}

func collectMetrics(client monitoring.MonitoringClient, gauge *prometheus.GaugeVec,
    tenants TenancyConfig, config MetricConfig,
) {
    for _, ten := range tenants.Tenancies {
        client.SetRegion(ten.Region)
        now := time.Now().UTC()
        start := common.SDKTime{Time: now.Add(-1 * time.Minute)}
        end := common.SDKTime{Time: now}

        for _, ns := range config.Metrics {
            for _, name := range ns.Names {
                // Single metric query per call
                query := fmt.Sprintf("%s[1m].mean()", name)
                req := monitoring.SummarizeMetricsDataRequest{
                    CompartmentId:          common.String(ten.CompartmentID),
                    CompartmentIdInSubtree: common.Bool(true),
                    SummarizeMetricsDataDetails: monitoring.SummarizeMetricsDataDetails{
                        Namespace: common.String(ns.Namespace),
                        Query:     common.String(query),
                        StartTime: &start,
                        EndTime:   &end,
                    },
                }
                if ns.ResourceGroup != "" {
                    req.SummarizeMetricsDataDetails.ResourceGroup = common.String(ns.ResourceGroup)
                }
                if ns.Resolution != "" {
                    req.SummarizeMetricsDataDetails.Resolution = common.String(ns.Resolution)
                }

                resp, err := summarizeWithRetry(client, req)
                if err != nil {
                    log.Printf("Error querying %s in %s: %v", name, ns.Namespace, err)
                } else {
                    for _, item := range resp.Items {
                        if len(item.AggregatedDatapoints) == 0 {
                            continue
                        }
                        latest := item.AggregatedDatapoints[len(item.AggregatedDatapoints)-1]
                        dimKey, dimVal := "resourceId", item.Dimensions["resourceId"]
                        if dimVal == "" {
                            for k, v := range item.Dimensions {
                                if v != "" {
                                    dimKey, dimVal = k, v
                                    break
                                }
                            }
                        }
                        metricLabel := ""
                        if item.Name != nil {
                            metricLabel = *item.Name
                        }
                        gauge.With(prometheus.Labels{
                            "tenancy":         ten.Name,
                            "region":          ten.Region,
                            "namespace":       ns.Namespace,
                            "metric":          metricLabel,
                            "dimension_key":   dimKey,
                            "dimension_value": dimVal,
                        }).Set(*latest.Value)
                    }
                }
                // rate limit: max 10 TPS
                time.Sleep(100 * time.Millisecond)
            }
        }
    }
}

func main() {
    configPath := flag.String("config", "", "Path to OCI API config file")
    listen := flag.String("listen-address", ":8080", "Metrics listen address")
    flag.Parse()

    if *configPath == "" {
        fmt.Println("Missing required -config")
        os.Exit(1)
    }

    provider, err := common.ConfigurationProviderFromFile(*configPath, "")
    if err != nil {
        log.Fatalf("Unable to load OCI config: %v", err)
    }
    client, err := monitoring.NewMonitoringClientWithConfigurationProvider(provider)
    if err != nil {
        log.Fatalf("Error creating OCI client: %v", err)
    }

    tenants, metrics := loadConfigs()

    gauge := prometheus.NewGaugeVec(prometheus.GaugeOpts{
        Name: "oci_metric_value",
        Help: "OCI Monitoring metric value",
    }, []string{"tenancy", "region", "namespace", "metric", "dimension_key", "dimension_value"})
    registry := prometheus.NewRegistry()
    registry.MustRegister(gauge)

    go func() {
        for {
            collectMetrics(client, gauge, tenants, metrics)
            time.Sleep(1 * time.Minute)
        }
    }()

    http.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
    log.Printf("Exporter listening on %s", *listen)
    log.Fatal(http.ListenAndServe(*listen, nil))
}
