package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/promlog"
	"github.com/prometheus/common/promlog/flag"
	"github.com/prometheus/common/version"
	kingpin "gopkg.in/alecthomas/kingpin.v2"

	"github.com/percona/azure_metrics_exporter/config"
)

var (
	sc = &config.SafeConfig{
		C: &config.Config{},
	}
	ac                    = NewAzureClient()
	configFile            = kingpin.Flag("config.file", "Azure exporter configuration file.").Default("azure.yml").String()
	listenAddress         = kingpin.Flag("web.listen-address", "The address to listen on for HTTP requests.").Default(":9276").String()
	listMetricDefinitions = kingpin.Flag("list.definitions", "List available metric definitions for the given resources and exit.").Bool()
	listMetricNamespaces  = kingpin.Flag("list.namespaces", "List available metric namespaces for the given resources and exit.").Bool()
	invalidMetricChars    = regexp.MustCompile("[^a-zA-Z0-9_:]")
	azureErrorDesc        = prometheus.NewDesc("azure_error", "Error collecting metrics", nil, nil)
	batchSize             = 20
	logger                = log.NewNopLogger()
)

func init() {
	prometheus.MustRegister(version.NewCollector("azure_exporter"))
}

// Collector generic collector type
type Collector struct{}

// Describe implemented with dummy data to satisfy interface.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- prometheus.NewDesc("dummy", "dummy", nil, nil)
}

type resourceMeta struct {
	resourceID      string
	resourceURL     string
	metricNamespace string
	metrics         string
	aggregations    []string
	resource        AzureResource
}

func (c *Collector) extractMetrics(ch chan<- prometheus.Metric, rm resourceMeta, httpStatusCode int, metricValueData AzureMetricValueResponse, publishedResources map[string]bool) {
	if httpStatusCode != 200 {
		level.Error(logger).Log(fmt.Sprintf("Received %d status for resource %s. %s", httpStatusCode, rm.resourceURL, metricValueData.APIError.Message))
		return
	}

	if len(metricValueData.Value) == 0 || len(metricValueData.Value[0].Timeseries) == 0 {
		level.Error(logger).Log(fmt.Sprintf("Metric %v not found at target %v\n", rm.metrics, rm.resourceURL))
		return
	}
	if len(metricValueData.Value[0].Timeseries[0].Data) == 0 {
		level.Error(logger).Log(fmt.Sprintf("No metric data returned for metric %v at target %v\n", rm.metrics, rm.resourceURL))
		return
	}

	for _, value := range metricValueData.Value {
		// Ensure Azure metric names conform to Prometheus metric name conventions
		metricName := strings.Replace(value.Name.Value, " ", "_", -1)
		metricName = strings.ToLower(metricName + "_" + value.Unit)
		metricName = strings.Replace(metricName, "/", "_per_", -1)
		if rm.metricNamespace != "" {
			metricName = strings.ToLower(rm.metricNamespace + "_" + metricName)
		}
		metricName = invalidMetricChars.ReplaceAllString(metricName, "_")

		var val float64
		if len(value.Timeseries) > 0 {
			metricValue := value.Timeseries[0].Data[len(value.Timeseries[0].Data)-1]
			labels := CreateResourceLabels(rm.resourceURL)

			if hasAggregation(rm.aggregations, "Total") {
				metricName = fmt.Sprintf("%s_total", metricName)
				val = metricValue.Total
			}
			if hasAggregation(rm.aggregations, "Average") {
				metricName = fmt.Sprintf("%s_average", metricName)
				val = metricValue.Average
			}
			if hasAggregation(rm.aggregations, "Minimum") {
				metricName = fmt.Sprintf("%s_min", metricName)
				val = metricValue.Minimum
			}
			if hasAggregation(rm.aggregations, "Minimum") {
				metricName = fmt.Sprintf("%s_max", metricName)
				val = metricValue.Maximum
			}

			alias := getAliasForMetricName(metricName)
			ch <- prometheus.MustNewConstMetric(
				prometheus.NewDesc(alias, alias, nil, labels),
				prometheus.GaugeValue,
				val,
			)
		}
	}

	if _, ok := publishedResources[rm.resource.ID]; !ok {
		infoLabels := CreateAllResourceLabelsFrom(rm)
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc("azure_resource_info", "Azure information available for resource", nil, infoLabels),
			prometheus.GaugeValue,
			1,
		)
		publishedResources[rm.resource.ID] = true
	}
}

func getAliasForMetricName(metricName string) string {
	switch metricName {
	// Our common metrics for nodes.
	case "cpu_percent_percent_average":
		return "node_cpu_average"
	case "network_bytes_egress_bytes_average":
		return "node_network_transmit_bytes_total"
	case "network_bytes_ingress_bytes_average":
		return "node_network_receive_bytes_total"
	case "storage_limit_bytes_average":
		return "node_filesystem_size_bytes"
	// Unique metrics for Azure.
	case "storage_used_bytes_average":
		return "azure_storage_used_bytes_average"
	case "storage_percent_percent_average":
		return "azure_storage_percent_average"
	case "memory_percent_percent_average":
		return "azure_memory_percent_average"
	default:
		return metricName
	}
}

func (c *Collector) batchCollectMetrics(ch chan<- prometheus.Metric, resources []resourceMeta) {
	var publishedResources = map[string]bool{}

	// collect metrics in batches
	for i := 0; i < len(resources); i += batchSize {
		j := i + batchSize

		// don't forget to add remainder resources
		if j > len(resources) {
			j = len(resources)
		}

		var urls []string
		for _, r := range resources[i:j] {
			urls = append(urls, r.resourceURL)
		}

		batchBody, err := ac.getBatchResponseBody(urls)
		if err != nil {
			ch <- prometheus.NewInvalidMetric(azureErrorDesc, err)
			return
		}

		var batchData AzureBatchMetricResponse
		err = json.Unmarshal(batchBody, &batchData)
		if err != nil {
			ch <- prometheus.NewInvalidMetric(azureErrorDesc, err)
			return
		}

		for k, resp := range batchData.Responses {
			c.extractMetrics(ch, resources[i+k], resp.HttpStatusCode, resp.Content, publishedResources)
		}
	}
}

func (c *Collector) batchLookupResources(resources []resourceMeta) ([]resourceMeta, error) {
	var updatedResources = resources
	// collect resource info in batches
	for i := 0; i < len(resources); i += batchSize {
		j := i + batchSize

		// don't forget to add remainder resources
		if j > len(resources) {
			j = len(resources)
		}

		var urls []string
		for _, r := range resources[i:j] {
			resourceType := GetResourceType(r.resourceURL)
			if resourceType == "" {
				return nil, fmt.Errorf("No type found for resource: %s", r.resourceID)
			}

			apiVersion := ac.APIVersions.findBy(resourceType)
			if apiVersion == "" {
				return nil, fmt.Errorf("No api version found for type: %s", resourceType)
			}

			subscription := fmt.Sprintf("subscriptions/%s", sc.C.Credentials.SubscriptionID)
			resourcesEndpoint := fmt.Sprintf("/%s/%s?api-version=%s", subscription, r.resourceID, apiVersion)

			urls = append(urls, resourcesEndpoint)
		}

		batchBody, err := ac.getBatchResponseBody(urls)
		if err != nil {
			return nil, err
		}

		var batchData AzureBatchLookupResponse
		err = json.Unmarshal(batchBody, &batchData)
		if err != nil {
			return nil, fmt.Errorf("Error unmarshalling response body: %v", err)
		}

		for k, resp := range batchData.Responses {
			updatedResources[i+k].resource = resp.Content
			updatedResources[i+k].resource.Subscription = sc.C.Credentials.SubscriptionID
		}
	}
	return updatedResources, nil
}

// Collect - collect results from Azure Montior API and create Prometheus metrics.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	if err := ac.refreshAccessToken(); err != nil {
		level.Error(logger).Log(err)
		ch <- prometheus.NewInvalidMetric(azureErrorDesc, err)
		return
	}

	var resources []resourceMeta
	var incompleteResources []resourceMeta

	for _, target := range sc.C.Targets {
		var rm resourceMeta

		metrics := []string{}
		for _, metric := range target.Metrics {
			metrics = append(metrics, metric.Name)
		}

		rm.resourceID = target.Resource
		rm.metricNamespace = target.MetricNamespace
		rm.metrics = strings.Join(metrics, ",")
		rm.aggregations = filterAggregations(target.Aggregations)
		rm.resourceURL = resourceURLFrom(target.Resource, rm.metricNamespace, rm.metrics, rm.aggregations)
		incompleteResources = append(incompleteResources, rm)
	}

	for _, resourceGroup := range sc.C.ResourceGroups {
		metrics := []string{}
		for _, metric := range resourceGroup.Metrics {
			metrics = append(metrics, metric.Name)
		}
		metricsStr := strings.Join(metrics, ",")

		filteredResources, err := ac.filteredListFromResourceGroup(resourceGroup)
		if err != nil {
			level.Error(logger).Log("Failed to get resources for resource group %s and resource types %s: %v",
				resourceGroup.ResourceGroup, resourceGroup.ResourceTypes, err)
			ch <- prometheus.NewInvalidMetric(azureErrorDesc, err)
			return
		}

		for _, f := range filteredResources {
			var rm resourceMeta
			rm.resourceID = f.ID
			rm.metricNamespace = resourceGroup.MetricNamespace
			rm.metrics = metricsStr
			rm.aggregations = filterAggregations(resourceGroup.Aggregations)
			rm.resourceURL = resourceURLFrom(f.ID, rm.metricNamespace, rm.metrics, rm.aggregations)
			rm.resource = f
			resources = append(resources, rm)
		}
	}

	resourcesCache := make(map[string][]byte)
	for _, resourceTag := range sc.C.ResourceTags {
		metrics := []string{}
		for _, metric := range resourceTag.Metrics {
			metrics = append(metrics, metric.Name)
		}
		metricsStr := strings.Join(metrics, ",")

		filteredResources, err := ac.filteredListByTag(resourceTag, resourcesCache)
		if err != nil {
			level.Error(logger).Log("Failed to get resources for tag name %s, tag value %s: %v",
				resourceTag.ResourceTagName, resourceTag.ResourceTagValue, err)
			ch <- prometheus.NewInvalidMetric(azureErrorDesc, err)
			return
		}

		for _, f := range filteredResources {
			var rm resourceMeta
			rm.resourceID = f.ID
			rm.metricNamespace = resourceTag.MetricNamespace
			rm.metrics = metricsStr
			rm.aggregations = filterAggregations(resourceTag.Aggregations)
			rm.resourceURL = resourceURLFrom(f.ID, rm.metricNamespace, rm.metrics, rm.aggregations)
			incompleteResources = append(incompleteResources, rm)
		}
	}

	completeResources, err := c.batchLookupResources(incompleteResources)
	if err != nil {
		level.Error(logger).Log("Failed to get resource info: %s", err)
		ch <- prometheus.NewInvalidMetric(azureErrorDesc, err)
		return
	}

	resources = append(resources, completeResources...)
	c.batchCollectMetrics(ch, resources)
}

func handler(w http.ResponseWriter, r *http.Request) {
	registry := prometheus.NewRegistry()
	collector := &Collector{}
	registry.MustRegister(collector)
	h := promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
	h.ServeHTTP(w, r)
}

func main() {
	kingpin.HelpFlag.Short('h')
	promlogConfig := &promlog.Config{}
	flag.AddFlags(kingpin.CommandLine, promlogConfig)
	kingpin.Version(version.Print("azure_exporter"))
	kingpin.Parse()
	logger = promlog.New(promlogConfig)
	if err := sc.ReloadConfig(*configFile); err != nil {
		level.Error(logger).Log("msg", "Error loading config", "error", err)
		os.Exit(1)
	}

	err := ac.getAccessToken()
	if err != nil {
		level.Error(logger).Log("msg", "Failed to get token", "error", err)
		os.Exit(1)
	}

	// Print list of available metric definitions for each resource to console if specified.
	if *listMetricDefinitions {
		results, err := ac.getMetricDefinitions()
		if err != nil {
			level.Error(logger).Log("msg", "Failed to fetch metric definitions", "error", err)
			os.Exit(1)
		}

		for k, v := range results {
			level.Info(logger).Log(fmt.Sprintf("Resource: %s\n\nAvailable Metrics:", k))
			for _, r := range v.MetricDefinitionResponses {
				level.Info(logger).Log("msg", fmt.Sprintf("- %s", r.Name.Value))
			}
		}
		os.Exit(0)
	}

	// Print list of available metric namespace for each resource to console if specified.
	if *listMetricNamespaces {
		results, err := ac.getMetricNamespaces()
		if err != nil {
			level.Error(logger).Log("msg", "Failed to fetch metric namespaces", "error", err)
		}

		for k, v := range results {
			level.Info(logger).Log(fmt.Sprintf("Resource: %s\n\nAvailable namespaces:", k))
			for _, namespace := range v.MetricNamespaceCollection {
				level.Info(logger).Log("msg", fmt.Sprintf("- %s", namespace.Properties.MetricNamespaceName))
			}
		}
		os.Exit(0)
	}

	err = ac.listAPIVersions()
	if err != nil {
		level.Error(logger).Log(err)
		os.Exit(1)
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
            <head>
            <title>Azure Exporter</title>
            </head>
            <body>
            <h1>Azure Exporter</h1>
						<p><a href="/metrics">Metrics</a></p>
            </body>
            </html>`))
	})

	http.HandleFunc("/metrics", handler)
	level.Info(logger).Log("msg", fmt.Sprintf("azure_metrics_exporter listening on port %v", *listenAddress))
	if err := http.ListenAndServe(*listenAddress, nil); err != nil {
		level.Error(logger).Log("msg", "Error starting HTTP server", "error", err)
		os.Exit(1)
	}
}
