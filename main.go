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
	prometheusMetricsAddress = kingpin.Flag("metrics-listen-address", "The address to listen on for Prometheus metrics requests.").Envar("PROMETHEUS_METRICS_PORT").Default(":9001").String()
	prometheusMetricsPath    = kingpin.Flag("metrics-path", "The path to listen for Prometheus metrics requests.").Envar("PROMETHEUS_METRICS_PATH").Default("/metrics").String()
	googleComputeProject     = kingpin.Flag("google-compute-project", "The Google Cloud project name to get quota for.").Envar("GCLOUD_PROJECT_NAME").String()

	// seed random number
	r = rand.New(rand.NewSource(time.Now().UnixNano()))

	// map with prometheus metrics
	gauges = make(map[string]prometheus.Gauge)
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

	// define channel and wait group to gracefully shutdown the application
	gracefulShutdown := make(chan os.Signal)
	signal.Notify(gracefulShutdown, syscall.SIGTERM, syscall.SIGINT)
	waitGroup := &sync.WaitGroup{}

	// watch gcloud quota
	go func(waitGroup *sync.WaitGroup) {
		// loop indefinitely
		for {
			log.Info().Msg("Fetching gcloud quota...")

			ctx := context.Background()
			client, err := google.DefaultClient(ctx, compute.CloudPlatformScope)
			if err != nil {
				log.Fatal().Err(err).Msg("Creating google cloud client failed")
			}

			computeService, err := compute.New(client)
			if err != nil {
				log.Fatal().Err(err).Msg("Creating google cloud service failed")
			}

			project, err := computeService.Projects.Get(*googleComputeProject).Context(ctx).Do()
			if err != nil {
				log.Fatal().Err(err).Msg("Creating gcloud service failed")
			}

			updatePrometheusTimelinesFromQuota(project.Quotas)

			// sleep random time between 22 and 37 seconds
			sleepTime := applyJitter(300)
			log.Info().Msgf("Sleeping for %v seconds...", sleepTime)
			time.Sleep(time.Duration(sleepTime) * time.Second)
		}
	}(waitGroup)

	signalReceived := <-gracefulShutdown
	log.Info().
		Msgf("Received signal %v. Waiting on running tasks to finish...", signalReceived)

	waitGroup.Wait()

	log.Info().Msg("Shutting down...")
}

func updatePrometheusTimelinesFromQuota(quotas []*compute.Quota) (err error) {

	for _, quota := range quotas {

		quotaName := casee.ToSnakeCase(quota.Metric)
		quotaLimitName := "estafette_gcloud_quota_" + quotaName + "_limit"
		quotaUsageName := "estafette_gcloud_quota_" + quotaName + "_usage"

		if _, ok := gauges[quotaLimitName]; !ok {
			// create and register gauge for limit value
			gauges[quotaLimitName] = prometheus.NewGauge(prometheus.GaugeOpts{
				Name: quotaLimitName,
				Help: fmt.Sprintf("The limit for quota %v.", quota.Metric),
				ConstLabels: map[string]string{
					"project": *googleComputeProject,
				},
			})
			prometheus.MustRegister(gauges[quotaLimitName])
		}

		// set the limit value
		gauges[quotaLimitName].Set(quota.Limit)

		if _, ok := gauges[quotaUsageName]; !ok {
			// create and register gauge for usage value
			gauges[quotaUsageName] = prometheus.NewGauge(prometheus.GaugeOpts{
				Name: quotaUsageName,
				Help: fmt.Sprintf("The usage for quota %v.", quota.Metric),
				ConstLabels: map[string]string{
					"project": *googleComputeProject,
				},
			})
			prometheus.MustRegister(gauges[quotaUsageName])
		}

		// set the limit value
		gauges[quotaUsageName].Set(quota.Usage)

	}

	log.Info().Interface("quotas", quotas).Msg("Quotas")

	return
}

func applyJitter(input int) (output int) {

	deviation := int(0.25 * float64(input))

	return input - deviation + r.Intn(2*deviation)
}
