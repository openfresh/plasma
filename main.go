package main

import (
	"crypto/tls"
	"net"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/net/context"

	"golang.org/x/sync/errgroup"

	"go.uber.org/zap"

	"net/http"

	"github.com/Yamashou/plasma/subscriber"
	"github.com/openfresh/plasma/config"
	"github.com/openfresh/plasma/log"
	"github.com/openfresh/plasma/metrics"
	"github.com/openfresh/plasma/pubsub"
	"github.com/openfresh/plasma/server"
)

func httpListener(logger *zap.Logger, config config.Config) net.Listener {
	l, err := net.Listen("tcp", ":"+config.Port)
	if err != nil {
		logger.Fatal("failed to http(https) listen",
			zap.Error(err),
			zap.String("port", config.Port),
		)
	}

	if config.TLS.CertFile != "" && config.TLS.KeyFile != "" {
		cer, err := tls.LoadX509KeyPair(config.TLS.CertFile, config.TLS.KeyFile)
		if err != nil {
			logger.Fatal("failed to load TLS credentials for TCP",
				zap.Error(err),
				zap.String("certFile", config.TLS.CertFile),
				zap.String("keyFile", config.TLS.KeyFile),
			)
		}

		logger.Info("enable TLS mode",
			zap.String("certFile", config.TLS.CertFile),
			zap.String("keyFile", config.TLS.KeyFile),
		)
		return tls.NewListener(l, &tls.Config{
			Certificates: []tls.Certificate{cer},
		})
	}

	logger.Info("non TLS mode")
	return l
}

func metricsListener(logger *zap.Logger, metricsPort string) net.Listener {
	l, err := net.Listen("tcp", ":"+metricsPort)
	if err != nil {
		logger.Fatal("failed to http listen(metrics)",
			zap.Error(err),
			zap.String("port", metricsPort),
		)
	}
	return l
}

func grpcListener(logger *zap.Logger, config config.Config) net.Listener {
	l, err := net.Listen("tcp", ":"+config.GrpcPort)
	if err != nil {
		logger.Fatal("failed to grpc listen",
			zap.Error(err),
			zap.String("grpc-port", config.GrpcPort),
		)
	}
	return l
}

func main() {
	config := config.New()

	accessLogger, err := log.NewLogger(config.AccessLog)
	if err != nil {
		panic(err)
	}
	errorLogger, err := log.NewLogger(config.ErrorLog)
	if err != nil {
		panic(err)
	}

	go func() {
		if err := http.ListenAndServe(config.Pprof.Host+":"+config.Pprof.Port, nil); err != nil {
			errorLogger.Fatal("failed to pprof http serve",
				zap.Error(err),
			)
		}
	}()

	l := httpListener(errorLogger, config)
	defer l.Close()
	gl := grpcListener(errorLogger, config)
	defer gl.Close()
	ml := metricsListener(errorLogger, config.MerticsPort)
	defer ml.Close()

	pubsuber := pubsub.NewPubSub()

	sub, err := subscriber.New(pubsuber, errorLogger, config)
	if err != nil {
		errorLogger.Fatal("failed to create subscriber",
			zap.Error(err),
			zap.String("type", config.Subscriber.Type),
			zap.Duration("mockDuration", config.Subscriber.Mock.Interval),
			zap.Object("redis", config.Subscriber.Redis),
		)
	}
	go func() {
		sub := sub
		if err := sub.Subscribe(); err != nil {
			errorLogger.Fatal("failed to subscribe",
				zap.String("type", config.Subscriber.Type),
				zap.Object("redis", config.Subscriber.Redis),
				zap.Error(err),
			)
		}
	}()

	// Start Metrics
	if config.Metrics.Type != "" {
		metrics, err := metrics.NewMetrics(config)
		if err != nil {
			errorLogger.Fatal("failed to create metrics",
				zap.Error(err),
			)
		}
		metrics.Start()
		defer metrics.Stop()
	}

	// For Native Client
	grpcServerOption := server.Option{
		PubSuber:     pubsuber,
		AccessLogger: accessLogger,
		ErrorLogger:  errorLogger,
		Config:       config,
	}

	grpcServer, err := server.NewGRPCServer(grpcServerOption)
	if err != nil {
		errorLogger.Fatal("failed to create gRPC server",
			zap.Error(err),
		)
	}

	// For Web Front End
	sseServerOption := server.Option{
		PubSuber:     pubsuber,
		AccessLogger: accessLogger,
		ErrorLogger:  errorLogger,
		Config:       config,
	}
	sseHandler, err := server.NewSSEHandler(sseServerOption)
	if err != nil {
		errorLogger.Fatal("failed to create SSE Handler",
			zap.Error(err),
		)
	}

	// For Metrics
	metricsHandler := server.NewMetricsHandler(server.Option{
		AccessLogger: accessLogger,
		ErrorLogger:  errorLogger,
		Config:       config,
	})

	// For Meta (HealthCheck)
	metaHandler := server.NewMetaHandler(server.Option{
		AccessLogger: accessLogger,
		ErrorLogger:  errorLogger,
		Config:       config,
	})

	metricsServer := &http.Server{
		Handler: metricsHandler,
	}

	httpServer := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			accept := r.Header.Get("Accept")
			if accept == "text/event-stream" {
				sseHandler.ServeHTTP(w, r)
			} else {
				metaHandler.ServeHTTP(w, r)
			}
		}),
	}

	// for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(
		sigCh,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGQUIT,
		syscall.SIGTERM,
	)
	go func() {
		<-sigCh

		eg := errgroup.Group{}
		eg.Go(func() error {
			errorLogger.Info("shutdown gRPC Server gracefully...")
			grpcServer.GracefulStop()
			return nil
		})
		eg.Go(func() error {
			errorLogger.Info("shutdown httpServer gracefully...")
			return httpServer.Shutdown(context.Background())
		})
		if err := eg.Wait(); err != nil {
			opErr, ok := err.(*net.OpError)

			// NOTE: Ignore errors that occur when closing the file descriptor because it is an assumed error.
			if ok && opErr.Op == "close" {
				return
			}
			errorLogger.Fatal("failed to shutdown gracefully",
				zap.Error(err),
			)
		}
	}()

	go func() {
		if err := grpcServer.Serve(gl); err != nil {
			errorLogger.Fatal("failed to gRPC serve",
				zap.Error(err),
			)
		}
	}()

	go func() {
		if err := metricsServer.Serve(ml); err != nil {
			errorLogger.Fatal("failed to metrics http serve",
				zap.Error(err),
			)
		}
	}()

	if err := httpServer.Serve(l); err != nil {
		errorLogger.Fatal("failed to HTTP serve",
			zap.Error(err),
		)
	}

}
