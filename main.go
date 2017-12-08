package main

import (
	"context"
	"fmt"
	stdlog "log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alecthomas/kingpin"
	"github.com/pinzolo/casee"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"golang.org/x/oauth2/google"
	compute "google.golang.org/api/compute/v1"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const annotationCloudflareDNS string = "estafette.io/cloudflare-dns"
const annotationCloudflareHostnames string = "estafette.io/cloudflare-hostnames"
const annotationCloudflareProxy string = "estafette.io/cloudflare-proxy"
const annotationCloudflareUseOriginRecord string = "estafette.io/cloudflare-use-origin-record"
const annotationCloudflareOriginRecordHostname string = "estafette.io/cloudflare-origin-record-hostname"

const annotationCloudflareState string = "estafette.io/cloudflare-state"

// CloudflareState represents the state of the service at Cloudflare
type CloudflareState struct {
	Enabled              string `json:"enabled"`
	Hostnames            string `json:"hostnames"`
	Proxy                string `json:"proxy"`
	UseOriginRecord      string `json:"useOriginRecord"`
	OriginRecordHostname string `json:"originRecordHostname"`
	IPAddress            string `json:"ipAddress"`
}

var (
	version   string
	branch    string
	revision  string
	buildDate string
	goVersion = runtime.Version()
)

var (
	// flags
	prometheusMetricsAddress = kingpin.Flag("metrics-listen-address", "The address to listen on for Prometheus metrics requests.").Envar("PROMETHEUS_METRICS_PORT").Default(":9101").String()
	prometheusMetricsPath    = kingpin.Flag("metrics-path", "The path to listen for Prometheus metrics requests.").Envar("PROMETHEUS_METRICS_PATH").Default("/metrics").String()
	googleComputeProject     = kingpin.Flag("google-compute-project", "The Google Cloud project ids to get quota for (optionally as comma-separated list).").Envar("GCLOUD_PROJECT_NAME").String()
	googleComputeRegions     = kingpin.Flag("google-compute-regions", "The Google Cloud regions to get quota for (optionally as comma-separated list).").Envar("GCLOUD_REGIONS").String()

	// seed random number
	r = rand.New(rand.NewSource(time.Now().UnixNano()))

	// map with prometheus metrics
	gauges = make(map[string]*prometheus.GaugeVec)
)

func main() {

	// parse command line parameters
	kingpin.Parse()

	// log as severity for stackdriver logging to recognize the level
	zerolog.LevelFieldName = "severity"

	// set some default fields added to all logs
	log.Logger = zerolog.New(os.Stdout).With().
		Timestamp().
		Str("app", "estafette-gcloud-quota-exporter").
		Str("version", version).
		Logger()

	// use zerolog for any logs sent via standard log library
	stdlog.SetFlags(0)
	stdlog.SetOutput(log.Logger)

	// log startup message
	log.Info().
		Str("branch", branch).
		Str("revision", revision).
		Str("buildDate", buildDate).
		Str("goVersion", goVersion).
		Msg("Starting estafette-gcloud-quota-exporter...")

	// define channel and wait group to gracefully shutdown the application
	gracefulShutdown := make(chan os.Signal)
	signal.Notify(gracefulShutdown, syscall.SIGTERM, syscall.SIGINT)
	waitGroup := &sync.WaitGroup{}

	ctx := context.Background()
	client, err := google.DefaultClient(ctx, compute.CloudPlatformScope)
	if err != nil {
		log.Fatal().Err(err).Msg("Creating google cloud client failed")
	}

	computeService, err := compute.New(client)
	if err != nil {
		log.Fatal().Err(err).Msg("Creating google cloud service failed")
	}

	// split projects to list
	projects := strings.Split(*googleComputeProject, ",")

	// split regions to list
	regions := strings.Split(*googleComputeRegions, ",")

	// fetch once, before setting up prometheus handler
	fetchQuota(ctx, computeService, projects, regions)

	// start prometheus
	go func() {
		log.Debug().
			Str("port", *prometheusMetricsAddress).
			Msg("Serving Prometheus metrics...")

		http.Handle(*prometheusMetricsPath, promhttp.Handler())

		if err := http.ListenAndServe(*prometheusMetricsAddress, nil); err != nil {
			log.Fatal().Err(err).Msg("Starting Prometheus listener failed")
		}
	}()

	// watch gcloud quota
	go func(waitGroup *sync.WaitGroup) {
		// loop indefinitely
		for {
			// sleep random time between 60s +- 25%
			sleepTime := applyJitter(60)
			log.Info().Msgf("Sleeping for %v seconds...", sleepTime)
			time.Sleep(time.Duration(sleepTime) * time.Second)

			fetchQuota(ctx, computeService, projects, regions)
		}
	}(waitGroup)

	signalReceived := <-gracefulShutdown
	log.Info().
		Msgf("Received signal %v. Waiting on running tasks to finish...", signalReceived)

	waitGroup.Wait()

	log.Info().Msg("Shutting down...")
}

func fetchQuota(ctx context.Context, computeService *compute.Service, projects, regions []string) {
	log.Info().Msg("Fetching gcloud quota...")

	for _, project := range projects {

		p, err := computeService.Projects.Get(project).Context(ctx).Do()
		if err != nil {
			log.Fatal().Err(err).Msgf("Retrieving project detail for project %v failed", project)
		}

		updatePrometheusTimelinesFromQuota(p.Quotas, project, "")

		for _, region := range regions {
			r, err := computeService.Regions.Get(project, region).Context(ctx).Do()
			if err != nil {
				log.Fatal().Err(err).Msgf("Retrieving region detail for project %v and region %v failed", project, region)
			}

			updatePrometheusTimelinesFromQuota(r.Quotas, project, region)
		}
	}
}

func updatePrometheusTimelinesFromQuota(quotas []*compute.Quota, project, region string) (err error) {

	for _, quota := range quotas {

		prefix := "estafette_gcloud_quota_"
		labels := []string{"project", "region"}
		if region == "" {
			prefix += "global_"
			labels = []string{"project"}
		}

		quotaName := casee.ToSnakeCase(quota.Metric)
		quotaLimitName := prefix + quotaName + "_limit"
		quotaUsageName := prefix + quotaName + "_usage"

		if _, ok := gauges[quotaLimitName]; !ok {
			// create and register gauge for limit value
			gauges[quotaLimitName] = prometheus.NewGaugeVec(prometheus.GaugeOpts{
				Name: quotaLimitName,
				Help: fmt.Sprintf("The limit for quota %v.", quota.Metric),
			}, labels)
			prometheus.MustRegister(gauges[quotaLimitName])
		}

		// set the limit value
		if region == "" {
			gauges[quotaLimitName].WithLabelValues(project).Add(quota.Limit)
		} else {
			gauges[quotaLimitName].WithLabelValues(project, region).Add(quota.Limit)
		}

		if _, ok := gauges[quotaUsageName]; !ok {
			// create and register gauge for usage value
			gauges[quotaUsageName] = prometheus.NewGaugeVec(prometheus.GaugeOpts{
				Name: quotaUsageName,
				Help: fmt.Sprintf("The usage for quota %v.", quota.Metric),
			}, labels)
			prometheus.MustRegister(gauges[quotaUsageName])
		}

		// set the usage value
		if region == "" {
			gauges[quotaUsageName].WithLabelValues(project).Add(quota.Usage)
		} else {
			gauges[quotaUsageName].WithLabelValues(project, region).Add(quota.Usage)
		}
	}

	log.Info().Interface("quotas", quotas).Msgf("Quotas for project %v and region %v", project, region)

	return
}

func applyJitter(input int) (output int) {

	deviation := int(0.25 * float64(input))

	return input - deviation + r.Intn(2*deviation)
}
