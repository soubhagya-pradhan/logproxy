package main

import (
	"os"
	"os/signal"

	"github.com/loafoe/go-rabbitmq"
	"github.com/philips-software/logproxy/handlers"
	log "github.com/sirupsen/logrus"

	"github.com/joho/godotenv"
	"github.com/labstack/echo"

	"net/http"
	_ "net/http/pprof"
)

var commit = "deadbeaf"
var release = "v1.1.0"
var buildVersion = release + "-" + commit

func init() {
	goEnv := os.Getenv("GOENV")
	if goEnv != "" {
		err := godotenv.Load(goEnv + ".env")
		if err != nil {
			log.Errorf("init error: %v\n", err.Error())
		}
	} else {
		_ = godotenv.Load("development.env")
	}
}

func consumerTag() string {
	return "logproxy"
}

func main() {

	// Echo framework
	e := echo.New()
	logger := log.New()
	logger.Infof("logproxy %s booting", buildVersion)

	// Health
	healthHandler := handlers.HealthHandler{}
	e.GET("/health", healthHandler.Handler())
	e.GET("/api/version", handlers.VersionHandler(buildVersion))

	// PHLogger
	phLogger, err := handlers.NewPHLogger(logger)
	if err != nil {
		logger.Errorf("Failed to setup PHLogger: %s", err)
		os.Exit(1)
	}

	// Syslog
	syslogHandler, err := handlers.NewSyslogHandler(os.Getenv("TOKEN"), logger)
	if err != nil {
		logger.Errorf("Failed to setup SyslogHandler: %s", err)
		os.Exit(1)
	}
	e.POST("/syslog/drain/:token", syslogHandler.Handler())

	// RabbitMQ
	consumer, err := rabbitmq.NewConsumer(rabbitmq.Config{
		RoutingKey:   handlers.RoutingKey,
		Exchange:     handlers.Exchange,
		ExchangeType: "topic",
		Durable:      false,
		AutoDelete:   true,
		QueueName:    phLogger.RFC5424QueueName(),
		CTag:         consumerTag(),
		HandlerFunc:  phLogger.RFC5424Worker,
	})
	if err != nil {
		logger.Errorf("Failed to create consumer: %v", err)
		os.Exit(2)
	}
	err = consumer.Start()
	if err != nil {
		logger.Errorf("Failed to start consumer: %v", err)
		os.Exit(2)
	}

	// Setup a channel to receive a signal
	done := make(chan os.Signal, 1)

	// Notify this channel when a SIGINT is received
	signal.Notify(done, os.Interrupt)

	// Fire off a goroutine to loop until that channel receives a signal.
	// When a signal is received simply exit the program
	go func() {
		for range done {
			logger.Error("Exiting because of CTRL-C")
			os.Exit(0)
		}
	}()

	go func() {
		logger.Info("Start pprof on localhost:6060")
		err := http.ListenAndServe("localhost:6060", nil)
		if err != nil {
			logger.Errorf("pprof not started: %v", err)
		}
	}()

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	listenString := ":" + port
	logger.Infof("Listening on %s", listenString)

	if err := e.Start(listenString); err != nil {
		logger.Errorf(err.Error())
	}
}
