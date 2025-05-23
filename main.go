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

// Tenancy represents a single OCI tenancy configuration.
type Tenancy struct {
    Name          string `yaml:"name"`
    TenancyID     string `yaml:"tenancy_id"`
    CompartmentID string `yaml:"compartment_id"`
    Region        string `yaml:"region"`
}

type TenancyConfig struct {
    Tenancies []Tenancy `yaml:"tenancies"`
}

// MetricNamespace holds namespace and list of metric names, optional resource group and resolution.
type MetricNamespace struct {
    Namespace     string   `yaml:"namespace"`
    Names         []string `yaml:"names"`
    ResourceGroup string   `yaml:"resource_group,omitempty"`
    Resolution    string   `yaml:"resolution,omitempty"`
}

type MetricConfig struct {
    Metrics []MetricNamespace `yaml:"metrics"`
}

// ociMetric is a Prometheus gauge for OCI metrics, labeled by tenancy, region, namespace, metric, resource_id, resource_display_name.
var ociMetric = prometheus.NewGaugeVec(
    prometheus.GaugeOpts{
        Name: "oci_metric_value",
        Help: "OCI Monitoring metric value",
    },
    []string{"tenancy", "region", "namespace", "metric", "resource_id", "resource_display_name"},
)

func init() {
    // Register only the OCI metric gauge in the default registry
    prometheus.MustRegister(ociMetric)
}

// loadConfigs reads tenants.yaml and metrics.yaml from config directory.
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

// summarizeWithRetry retries up to 3 times on HTTP 429 with exponential backoff.
func summarizeWithRetry(client monitoring.MonitoringClient, req monitoring.SummarizeMetricsDataRequest) (monitoring.SummarizeMetricsDataResponse, error) {
    var resp monitoring.SummarizeMetricsDataResponse
    var err error
    for attempt := 0; attempt < 3; attempt++ {
        resp, err = client.SummarizeMetricsData(context.Background(), req)
        if err == nil || !strings.Contains(err.Error(), "TooManyRequests") {
            return resp, err
        }
        backoff := time.Duration(1<<attempt) * time.Second
        log.Printf("TooManyRequests, backing off %v", backoff)
        time.Sleep(backoff)
    }
    return resp, err
}

// collectMetrics queries  metric individually and set gauge.
func collectMetrics(client monitoring.MonitoringClient, tenants TenancyConfig, config MetricConfig) {
    for _, ten := range tenants.Tenancies {
        client.SetRegion(ten.Region)
        now := time.Now().UTC()
        start := common.SDKTime{Time: now.Add(-1 * time.Minute)}
        end := common.SDKTime{Time: now}

        for _, ns := range config.Metrics {
            for _, name := range ns.Names {
                // single-metric MQL
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

                        // Extract resource labels
                        resID := item.Dimensions["resourceId"]
                        dispName := item.Dimensions["resourceDisplayName"]
                        metricLabel := name
                        if item.Name != nil {
                            metricLabel = *item.Name
                        }

                        ociMetric.With(prometheus.Labels{
                            "tenancy":               ten.Name,
                            "region":                ten.Region,
                            "namespace":             ns.Namespace,
                            "metric":                metricLabel,
                            "resource_id":           resID,
                            "resource_display_name": dispName,
                        }).Set(*latest.Value)
                    }
                }
                // Throttle to max 10 TPS
                time.Sleep(100 * time.Millisecond)
            }
        }
    }
}

func main() {
    cfg := flag.String("config", "", "Path to OCI config file")
    listen := flag.String("listen-address", ":8080", "Metrics listen address")
    flag.Parse()

    if *cfg == "" {
        fmt.Println("Missing required -config flag")
        os.Exit(1)
    }
    provider, err := common.ConfigurationProviderFromFile(*cfg, "")
    if err != nil {
        log.Fatalf("Failed loading OCI config: %v", err)
    }
    client, err := monitoring.NewMonitoringClientWithConfigurationProvider(provider)
    if err != nil {
        log.Fatalf("Failed creating Monitoring client: %v", err)
    }

    tenants, metricsCfg := loadConfigs()

    // Start collection
    go func() {
        for {
            collectMetrics(client, tenants, metricsCfg)
            time.Sleep(1 * time.Minute)
        }
    }()

    // Expose metrics
    http.Handle("/metrics", promhttp.Handler())
    log.Printf("Exporter listening on %s", *listen)
    log.Fatal(http.ListenAndServe(*listen, nil))
}
