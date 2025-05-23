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

func collectMetrics(client monitoring.MonitoringClient, gauge *prometheus.GaugeVec, tenants TenancyConfig, config MetricConfig) {
    for _, t := range tenants.Tenancies {
        client.SetRegion(t.Region)
        now := time.Now().UTC()
        start := common.SDKTime{Time: now.Add(-1 * time.Minute)}
        end := common.SDKTime{Time: now}

        for _, ns := range config.Metrics {
            if len(ns.Names) == 0 {
                continue
            }

            // grouped MQL query for this namespace
            var parts []string
            for _, m := range ns.Names {
                parts = append(parts, fmt.Sprintf("%s[1m].mean()", m))
            }
            queryText := strings.Join(parts, ",")

            req := monitoring.SummarizeMetricsDataRequest{
                CompartmentId:          common.String(t.CompartmentID),
                CompartmentIdInSubtree: common.Bool(true),
                SummarizeMetricsDataDetails: monitoring.SummarizeMetricsDataDetails{
                    Namespace: common.String(ns.Namespace),
                    Query:     common.String(queryText),
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
                log.Printf("Error querying namespace %s: %v", ns.Namespace, err)
            } else {
                for _, item := range resp.Items {
                    if len(item.AggregatedDatapoints) == 0 {
                        continue
                    }
                    latest := item.AggregatedDatapoints[len(item.AggregatedDatapoints)-1]

                    //  dimension label
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
                        "tenancy":         t.Name,
                        "region":          t.Region,
                        "namespace":       ns.Namespace,
                        "metric":          metricLabel,
                        "dimension_key":   dimKey,
                        "dimension_value": dimVal,
                    }).Set(*latest.Value)
                }
            }

            // Rate limit: max 10 TPS
            time.Sleep(100 * time.Millisecond)
        }
    }
}

func main() {
    configPath := flag.String("config", "", "Path to OCI config file")
    listenAddr := flag.String("listen-address", ":8080", "Metrics listen address")
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
        log.Fatalf("Failed to create monitoring client: %v", err)
    }

    tenants, metricsCfg := loadConfigs()

    // custom registry to expose only OCI metrics
    gauge := prometheus.NewGaugeVec(prometheus.GaugeOpts{
        Name: "oci_metric_value",
        Help: "OCI Monitoring metric value",
    }, []string{"tenancy", "region", "namespace", "metric", "dimension_key", "dimension_value"})
    registry := prometheus.NewRegistry()
    registry.MustRegister(gauge)

    go func() {
        for {
            collectMetrics(client, gauge, tenants, metricsCfg)
            time.Sleep(1 * time.Minute)
        }
    }()

    http.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
    log.Printf("Exporter listening on %s", *listenAddr)
    log.Fatal(http.ListenAndServe(*listenAddr, nil))
}
