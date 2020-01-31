package main

import (
	"context"
	"math/rand"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/alecthomas/kingpin"
	foundation "github.com/estafette/estafette-foundation"
	"github.com/fsnotify/fsnotify"
	"github.com/pinzolo/casee"
	"github.com/rs/zerolog/log"
	"golang.org/x/oauth2/google"
	compute "google.golang.org/api/compute/v1"

	"github.com/prometheus/client_golang/prometheus"
)

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
	appgroup  string
	app       string
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
	googleComputeProjects    = kingpin.Flag("google-compute-projects", "The Google Cloud project ids to get quota for (optionally as comma-separated list).").Envar("GCLOUD_PROJECTS").String()
	googleComputeRegions     = kingpin.Flag("google-compute-regions", "The Google Cloud regions to get quota for (optionally as comma-separated list).").Envar("GCLOUD_REGIONS").String()

	// seed random number
	r = rand.New(rand.NewSource(time.Now().UnixNano()))

	// create gauge for global limit value
	globalQuotaLimit = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "estafette_gcloud_global_quota_limit",
		Help: "The limit for global quota.",
	}, []string{"project", "metric"})

	// create gauge for global usage value
	globalQuotaUsage = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "estafette_gcloud_global_quota_usage",
		Help: "The usage for global quota.",
	}, []string{"project", "metric"})

	// create gauge for regional limit value
	regionalQuotaLimit = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "estafette_gcloud_regional_quota_limit",
		Help: "The limit for regional quota.",
	}, []string{"project", "region", "metric"})

	// create gauge for regional usage value
	regionalQuotaUsage = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "estafette_gcloud_regional_quota_usage",
		Help: "The usage for regional quota.",
	}, []string{"project", "region", "metric"})
)

func init() {
	prometheus.MustRegister(globalQuotaLimit)
	prometheus.MustRegister(globalQuotaUsage)
	prometheus.MustRegister(regionalQuotaLimit)
	prometheus.MustRegister(regionalQuotaUsage)
}

func main() {

	// parse command line parameters
	kingpin.Parse()

	// init log format from envvar ESTAFETTE_LOG_FORMAT
	foundation.InitLoggingFromEnv(foundation.NewApplicationInfo(appgroup, app, version, branch, revision, buildDate))

	// init /liveness endpoint
	foundation.InitLiveness()

	foundation.InitMetrics()

	ctx := context.Background()
	client, err := google.DefaultClient(ctx, compute.CloudPlatformScope)
	if err != nil {
		log.Fatal().Err(err).Msg("Creating google cloud client failed")
	}

	computeService, err := compute.New(client)
	if err != nil {
		log.Fatal().Err(err).Msg("Creating google cloud service failed")
	}

	foundation.WatchForFileChanges(os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"), func(event fsnotify.Event) {
		// reinitialize parts making use of the mounted data

		client, err = google.DefaultClient(ctx, compute.CloudPlatformScope)
		if err != nil {
			log.Fatal().Err(err).Msg("Creating google cloud client failed")
		}

		computeService, err = compute.New(client)
		if err != nil {

		}
	})

	gracefulShutdown, waitGroup := foundation.InitGracefulShutdownHandling()

	// split projects to list
	projects := strings.Split(*googleComputeProjects, ",")

	// split regions to list
	regions := strings.Split(*googleComputeRegions, ",")

	// watch gcloud quota
	go func(waitGroup *sync.WaitGroup) {
		// loop indefinitely
		for {
			fetchQuota(ctx, computeService, projects, regions)

			// sleep random time between 60s +- 25%
			sleepTime := applyJitter(60)
			log.Info().Msgf("Sleeping for %v seconds...", sleepTime)
			time.Sleep(time.Duration(sleepTime) * time.Second)
		}
	}(waitGroup)

	foundation.HandleGracefulShutdown(gracefulShutdown, waitGroup)
}

func fetchQuota(ctx context.Context, computeService *compute.Service, projects, regions []string) {

	log.Info().Msgf("Fetching gcloud quota for projects %v and regions %v...", projects, regions)

	for _, project := range projects {

		p, err := computeService.Projects.Get(project).Context(ctx).Do()
		if err != nil {
			log.Fatal().Err(err).Msgf("Retrieving project detail for project %v failed", project)
		}

		updateGlobalQuota(p.Quotas, project)

		for _, region := range regions {
			r, err := computeService.Regions.Get(project, region).Context(ctx).Do()
			if err != nil {
				log.Fatal().Err(err).Msgf("Retrieving region detail for project %v and region %v failed", project, region)
			}

			updateRegionalQuota(r.Quotas, project, region)
		}
	}
}

func updateGlobalQuota(quotas []*compute.Quota, project string) (err error) {

	for _, quota := range quotas {

		metricName := casee.ToSnakeCase(quota.Metric)

		globalQuotaLimit.WithLabelValues(project, metricName).Set(quota.Limit)
		globalQuotaUsage.WithLabelValues(project, metricName).Set(quota.Usage)

	}

	return
}

func updateRegionalQuota(quotas []*compute.Quota, project, region string) (err error) {

	for _, quota := range quotas {

		metricName := casee.ToSnakeCase(quota.Metric)

		regionalQuotaLimit.WithLabelValues(project, region, metricName).Set(quota.Limit)
		regionalQuotaUsage.WithLabelValues(project, region, metricName).Set(quota.Usage)

	}

	return
}

func applyJitter(input int) (output int) {

	deviation := int(0.25 * float64(input))

	return input - deviation + r.Intn(2*deviation)
}
