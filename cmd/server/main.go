package main

import (
	"crypto/tls"
	"io/ioutil"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/3scale/3scale-istio-adapter/pkg/threescale"
	"github.com/3scale/3scale-istio-adapter/pkg/threescale/authorizer"
	"github.com/3scale/3scale-istio-adapter/pkg/threescale/metrics"
	"github.com/spf13/viper"

	"google.golang.org/grpc/grpclog"

	"istio.io/istio/pkg/log"
)

var version string

const (
	defaultSystemCacheRetries                = 1
	defaultSystemCacheTTLSeconds             = 300
	defaultSystemCacheRefreshIntervalSeconds = 180
	defaultSystemCacheSize                   = 1000
)

func init() {
	viper.SetEnvPrefix("threescale")
	viper.BindEnv("log_level")
	viper.BindEnv("log_json")
	viper.BindEnv("log_grpc")
	viper.BindEnv("listen_addr")
	viper.BindEnv("report_metrics")
	viper.BindEnv("metrics_port")

	viper.BindEnv("cache_ttl_seconds")
	viper.BindEnv("cache_refresh_seconds")
	viper.BindEnv("cache_entries_max")

	viper.BindEnv("client_timeout_seconds")
	viper.BindEnv("allow_insecure_conn")

	viper.BindEnv("grpc_conn_max_seconds")

	viper.BindEnv("use_cached_backend")

	configureLogging()
}

func configureLogging() {
	options := log.DefaultOptions()
	loglevel := viper.GetString("log_level")
	parsedLogLevel := stringToLogLevel(loglevel)
	options.SetOutputLevel(log.DefaultScopeName, parsedLogLevel)
	options.JSONEncoding = viper.GetBool("log_json")

	if !viper.GetBool("log_grpc") {
		options.LogGrpc = false
		grpclog.SetLoggerV2(
			grpclog.NewLoggerV2WithVerbosity(ioutil.Discard, ioutil.Discard, ioutil.Discard, 0),
		)
	}

	log.Configure(options)
}

func stringToLogLevel(loglevel string) log.Level {

	stringToLevel := map[string]log.Level{
		"debug": log.DebugLevel,
		"info":  log.InfoLevel,
		"warn":  log.WarnLevel,
		"error": log.ErrorLevel,
		"none":  log.NoneLevel,
	}

	if val, ok := stringToLevel[strings.ToLower(loglevel)]; ok {
		return val
	}
	return log.InfoLevel
}

func parseMetricsConfig() *metrics.Reporter {
	if !viper.IsSet("report_metrics") || !viper.GetBool("report_metrics") {
		return nil
	}

	var port int
	if viper.IsSet("metrics_port") {
		port = viper.GetInt("metrics_port")
	} else {
		port = 8080
	}

	return metrics.NewMetricsReporter(true, port)
}

func parseClientConfig() *http.Client {
	c := &http.Client{
		// Setting some sensible default here for http timeouts
		Timeout: time.Duration(time.Second * 10),
	}

	if viper.IsSet("client_timeout_seconds") {
		c.Timeout = time.Duration(viper.GetInt("client_timeout_seconds")) * time.Second
	}

	if viper.IsSet("allow_insecure_conn") {
		tr := &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: viper.GetBool("allow_insecure_conn")},
		}
		c.Transport = tr
	}

	return c
}

func createSystemCache() *authorizer.SystemCache {
	cacheTTL := defaultSystemCacheTTLSeconds
	cacheEntriesMax := defaultSystemCacheSize
	cacheUpdateRetries := defaultSystemCacheRetries
	cacheRefreshInterval := defaultSystemCacheRefreshIntervalSeconds

	if viper.IsSet("cache_ttl_seconds") {
		cacheTTL = viper.GetInt("cache_ttl_seconds")
	}

	if viper.IsSet("cache_refresh_seconds") {
		cacheRefreshInterval = viper.GetInt("cache_refresh_seconds")
	}

	if viper.IsSet("cache_entries_max") {
		cacheEntriesMax = viper.GetInt("cache_entries_max")
	}

	if viper.IsSet("cache_refresh_retries") {
		cacheUpdateRetries = viper.GetInt("cache_refresh_retries")
	}

	config := authorizer.SystemCacheConfig{
		MaxSize:               cacheEntriesMax,
		NumRetryFailedRefresh: cacheUpdateRetries,
		RefreshInterval:       time.Duration(cacheRefreshInterval) * time.Second,
		TTL:                   time.Duration(cacheTTL) * time.Second,
	}

	return authorizer.NewSystemCache(config, make(chan struct{}))
}

func createBackendConfig() authorizer.BackendConfig {
	logger := log.FindScope(log.DefaultScopeName)
	if viper.GetBool("use_cached_backend") {
		return authorizer.BackendConfig{
			EnableCaching:      true,
			CacheFlushInterval: time.Second * 15,
			Logger:             logger,
		}
	}
	return authorizer.BackendConfig{
		Logger: logger,
	}
}

func main() {
	var addr string

	if viper.IsSet("listen_addr") {
		addr = viper.GetString("listen_addr")
	} else {
		addr = "0"
	}

	grpcKeepAliveFor := time.Minute
	if viper.IsSet("grpc_conn_max_seconds") {
		grpcKeepAliveFor = time.Second * time.Duration(viper.GetInt("grpc_conn_max_seconds"))
	}

	authorizer, err := authorizer.NewManager(
		authorizer.NewClientBuilder(parseClientConfig()),
		createSystemCache(),
		createBackendConfig(),
	)
	if err != nil {
		log.Fatalf("Unable to create authorizer: %v", err)
	}

	adapterConfig := threescale.NewAdapterConfig(authorizer, grpcKeepAliveFor)

	s, err := threescale.NewThreescale(addr, adapterConfig)
	if err != nil {
		log.Fatalf("Unable to start sever: %v", err)
	}

	shutdown := make(chan error, 1)
	go func() {
		if version == "" {
			version = "undefined"
		}
		log.Infof("Starting server version %s", version)
		s.Run(shutdown)
	}()

	sigC := make(chan os.Signal, 1)
	signal.Notify(sigC, syscall.SIGTERM, syscall.SIGINT)

	for {
		select {
		case sig := <-sigC:
			log.Infof("\n%s received. Attempting graceful shutdown\n", sig.String())
			authorizer.Shutdown()
			err := s.Close()
			if err != nil {
				log.Fatalf("Error calling graceful shutdown")
			}

		case err = <-shutdown:
			if err != nil {
				log.Fatalf("gRPC server has shut down: err %v", err)
			}

			log.Info("gRPC server has shut down gracefully")
		}
	}
}
