package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"

	zipkinReporter "github.com/openzipkin/zipkin-go/reporter"
	"github.com/philips-software/go-hsdp-api/iam"

	"github.com/philips-software/logproxy/queue"
	"github.com/philips-software/logproxy/shared"

	"github.com/philips-software/go-hsdp-api/logging"
	"github.com/philips-software/logproxy/handlers"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"

	"github.com/labstack/echo/v4"

	"net/http"
	_ "net/http/pprof"

	"github.com/labstack/echo-contrib/zipkintracing"
	"github.com/openzipkin/zipkin-go"
	zipkinHttpReporter "github.com/openzipkin/zipkin-go/reporter/http"
)

var commit = "deadbeaf"
var release = "v1.2.2"
var buildVersion = release + "-" + commit

func main() {
	e := make(chan *echo.Echo, 1)
	os.Exit(realMain(e))
}

func realMain(echoChan chan<- *echo.Echo) int {
	logger := log.New()

	viper.SetEnvPrefix("logproxy")
	viper.SetDefault("syslog", true)
	viper.SetDefault("ironio", false)
	viper.SetDefault("queue", "rabbitmq")
	viper.SetDefault("plugindir", "")
	viper.SetDefault("delivery", "hsdp")
	viper.SetDefault("transport_url", "")
	viper.SetDefault("region", "us-east")
	viper.SetDefault("env", "client-test")
	viper.SetDefault("service_id", "")
	viper.SetDefault("service_private_key", "")
	viper.AutomaticEnv()

	enableIronIO := viper.GetBool("ironio")
	enableSyslog := viper.GetBool("syslog")
	queueType := viper.GetString("queue")
	deliveryType := viper.GetString("delivery")
	token := os.Getenv("TOKEN")
	enableDebug := os.Getenv("DEBUG") == "true"
	transportURL := viper.GetString("transport_url")

	logger.Infof("logproxy %s booting", buildVersion)
	if !enableIronIO && !enableSyslog {
		logger.Errorf("both syslog and ironio drains are disabled")
		return 1
	}
	// Echo framework
	e := echo.New()

	// Tracing
	endpoint, err := zipkin.NewEndpoint("echo-service", "")
	if err != nil {
		e.Logger.Fatalf("error creating zipkin endpoint: %s", err.Error())
	}
	reporter := zipkinReporter.NewNoopReporter()
	if transportURL != "" {
		reporter = zipkinHttpReporter.NewReporter(transportURL)
	}
	traceTags := make(map[string]string)
	traceTags["app"] = "logproxy"
	tracer, err := zipkin.NewTracer(reporter,
		zipkin.WithLocalEndpoint(endpoint),
		zipkin.WithTags(traceTags),
		zipkin.WithSampler(zipkin.AlwaysSample))

	// Middleware
	if err == nil {
		e.Use(zipkintracing.TraceServer(tracer))
	}
	// Plugin Manager
	homeDir, _ := os.UserHomeDir()
	pluginExePath, _ := os.Executable()
	pluginManager := &shared.PluginManager{
		PluginDirs: []string{
			filepath.Join(homeDir, ".logproxy/plugins"),
			filepath.Dir(pluginExePath),
		},
	}
	if pluginDir := viper.GetString("plugindir"); pluginDir != "" {
		pluginManager.PluginDirs = append(pluginManager.PluginDirs, pluginDir)
	}
	if err := pluginManager.Discover(); err == nil {
		_ = pluginManager.LoadAll()
	}

	// Queue Type
	var messageQueue queue.Queue
	switch queueType {
	case "rabbitmq":
		messageQueue, err = queue.NewRabbitMQQueue()
		if err != nil {
			logger.Errorf("RabbitMQ queue error: %v", err)
			return 128
		}
		logger.Info("using RabbitMQ queue")
	default:
		messageQueue, _ = queue.NewChannelQueue()
		logger.Info("using internal channel queue")
	}

	healthHandler := handlers.HealthHandler{}
	e.GET("/health", healthHandler.Handler(tracer))
	e.GET("/api/version", handlers.VersionHandler(buildVersion))

	// Syslog
	if enableSyslog {
		syslogHandler, err := handlers.NewSyslogHandler(token, messageQueue)
		if err != nil {
			logger.Errorf("failed to setup SyslogHandler: %s", err)
			return 3
		}
		logger.Info("enabling /syslog/drain/:token")
		e.POST("/syslog/drain/:token", syslogHandler.Handler(tracer))
	}

	// IronIO
	if enableIronIO {
		ironIOHandler, err := handlers.NewIronIOHandler(token, messageQueue)
		if err != nil {
			logger.Errorf("Failed to setup IronIOHandler: %s", err)
			return 4
		}
		logger.Info("enabling /ironio/drain/:token")
		e.POST("/ironio/drain/:token", ironIOHandler.Handler(tracer))
	}

	setupPprof(logger)
	setupInterrupts(logger)

	// Consumer
	var done chan bool
	if done, err = messageQueue.Start(); err != nil {
		logger.Errorf("failed to start consumer: %v", err)
		return 5
	}

	// Worker
	sharedKey := os.Getenv("HSDP_LOGINGESTOR_KEY")
	sharedSecret := os.Getenv("HSDP_LOGINGESTOR_SECRET")
	baseURL := os.Getenv("HSDP_LOGINGESTOR_URL")
	productKey := os.Getenv("HSDP_LOGINGESTOR_PRODUCT_KEY")
	config := &logging.Config{
		SharedKey:    sharedKey,
		SharedSecret: sharedSecret,
		BaseURL:      baseURL,
		ProductKey:   productKey,
	}
	serviceID := viper.GetString("service_id")
	servicePrivateKey := viper.GetString("service_private_key")
	if serviceID != "" && servicePrivateKey != "" {
		debugLog := ""
		if enableDebug {
			debugLog = "/dev/stderr"
		}
		iamClient, err := iam.NewClient(nil, &iam.Config{
			Region:      viper.GetString("region"),
			Environment: viper.GetString("env"),
			DebugLog:    debugLog,
		})
		if err != nil {
			logger.Errorf("failed to create IAM client: %v", err)
			return 6
		}
		err = iamClient.ServiceLogin(iam.Service{
			ServiceID:  serviceID,
			PrivateKey: servicePrivateKey,
		})
		if err != nil {
			fmt.Printf("invalid service credentials: %v\n", err)
			return 7
		}
		config.IAMClient = iamClient
		config.SharedKey = ""
		config.SharedSecret = ""
	}

	doneWorker := make(chan bool)
	switch deliveryType {
	case "none":
		deliverer, _ := setupNoneDeliverer(logger, pluginManager, buildVersion)
		go deliverer.ResourceWorker(messageQueue, doneWorker, tracer)
	default:
		deliverer, err := setupHSDPDeliverer(http.DefaultClient, config, logger, pluginManager, buildVersion)
		if err != nil {
			logger.Errorf("failed to setup Deliverer: %s", err)
			return 20
		}
		go deliverer.ResourceWorker(messageQueue, doneWorker, tracer)
	}

	echoChan <- e
	exitCode := 0
	if err := e.Start(listenString()); err != nil {
		logger.Errorf(err.Error())
		exitCode = 6
	}
	done <- true
	doneWorker <- true
	return exitCode
}

type noneStorer struct {
}

func (n *noneStorer) StoreResources(_ []logging.Resource, _ int) (*logging.StoreResponse, error) {
	return &logging.StoreResponse{
		Response: &http.Response{
			StatusCode: http.StatusCreated,
		},
	}, nil
}

func setupNoneDeliverer(logger *log.Logger, manager *shared.PluginManager, buildVersion string) (*queue.Deliverer, error) {
	return queue.NewDeliverer(&noneStorer{}, logger, manager, buildVersion)
}

func setupHSDPDeliverer(httpClient *http.Client, config *logging.Config, logger *log.Logger, manager *shared.PluginManager, buildVersion string) (*queue.Deliverer, error) {
	storer, err := logging.NewClient(httpClient, config)
	if err != nil {
		return nil, err
	}
	return queue.NewDeliverer(storer, logger, manager, buildVersion)
}

func setupInterrupts(_ *log.Logger) {
	// Setup a channel to receive a signal
	done := make(chan os.Signal, 1)

	// Notify this channel when a SIGINT is received
	signal.Notify(done, os.Interrupt)

	// Fire off a goroutine to loop until that channel receives a signal.
	// When a signal is received simply exit the program
	go func() {
		for range done {
			os.Exit(0)
		}
	}()
}

func setupPprof(logger *log.Logger) {
	go func() {
		logger.Info("start pprof on localhost:6060")
		err := http.ListenAndServe("localhost:6060", nil)
		if err != nil {
			logger.Errorf("pprof not started: %v", err)
		}
	}()
}

func listenString() string {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	return (":" + port)
}
