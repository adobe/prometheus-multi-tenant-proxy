package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus-multi-tenant-proxy/api/v1alpha1"
	"github.com/prometheus-multi-tenant-proxy/internal/config"
	"github.com/prometheus-multi-tenant-proxy/internal/discovery"
	"github.com/prometheus-multi-tenant-proxy/internal/proxy"
	remote_write "github.com/prometheus-multi-tenant-proxy/internal/remote_write"
	"github.com/prometheus-multi-tenant-proxy/internal/tenant"
	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	configFile                  = flag.String("config", "/etc/prometheus-proxy/config.yaml", "Path to configuration file")
	port                        = flag.Int("port", 8080, "Port to listen on")
	logLevel                    = flag.String("log-level", "info", "Log level (debug, info, warn, error)")
	kubeconfig                  = flag.String("kubeconfig", "", "Path to kubeconfig file (optional, uses in-cluster config if not provided)")
	leaderElect                 = flag.Bool("leader-elect", true, "Enable leader election for the remote write controller. Must be true when running multiple replicas to avoid duplicate writes.")
	leaderElectionID            = flag.String("leader-election-id", "prometheus-multi-tenant-proxy-remote-write", "Name of the Lease object used for leader election")
	leaderElectionNamespace     = flag.String("leader-election-namespace", "", "Namespace for the leader election Lease (defaults to POD_NAMESPACE env var, then 'monitoring')")
)

func main() {
	flag.Parse()

	// Setup logging
	level, err := logrus.ParseLevel(*logLevel)
	if err != nil {
		logrus.Fatalf("Invalid log level: %v", err)
	}
	logrus.SetLevel(level)
	logrus.SetFormatter(&logrus.JSONFormatter{})

	logrus.Info("Starting Prometheus Multi-Tenant Proxy")

	// Load configuration
	cfg, err := config.Load(*configFile)
	if err != nil {
		logrus.Fatalf("Failed to load configuration: %v", err)
	}

	// Setup Kubernetes client
	k8sConfig, err := getKubernetesConfig(*kubeconfig)
	if err != nil {
		logrus.Fatalf("Failed to get Kubernetes config: %v", err)
	}

	k8sClient, err := kubernetes.NewForConfig(k8sConfig)
	if err != nil {
		logrus.Fatalf("Failed to create Kubernetes client: %v", err)
	}

	// Setup controller-runtime client for custom resources
	runtimeScheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(runtimeScheme))
	utilruntime.Must(v1alpha1.AddToScheme(runtimeScheme))

	crClient, err := client.New(k8sConfig, client.Options{Scheme: runtimeScheme})
	if err != nil {
		logrus.Fatalf("Failed to create controller-runtime client: %v", err)
	}

	// Initialize service discovery
	serviceDiscovery, err := discovery.NewKubernetesDiscovery(k8sClient, cfg.Discovery)
	if err != nil {
		logrus.Fatalf("Failed to initialize service discovery: %v", err)
	}

	// Initialize tenant manager
	tenantManager, err := tenant.NewManager(crClient, cfg.Tenants)
	if err != nil {
		logrus.Fatalf("Failed to initialize tenant manager: %v", err)
	}

	// Initialize remote write
	var remoteWriteController *remote_write.Controller
	// Always create the controller even if remote write is disabled
	// This ensures the /collected-metrics endpoint works even without MetricAccess resources
		remoteWriteController = remote_write.NewController(crClient, cfg.RemoteWrite, serviceDiscovery)
	
	// Start remote write controller only if enabled in config
	enableRemoteWrite := cfg.RemoteWrite.Enabled

	// Initialize proxy
	proxyHandler, err := proxy.NewHandler(cfg, serviceDiscovery, tenantManager, remoteWriteController)
	if err != nil {
		logrus.Fatalf("Failed to initialize proxy handler: %v", err)
	}

	// Setup HTTP server
	server := &http.Server{
		Addr:         fmt.Sprintf(":%d", *port),
		Handler:      proxyHandler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Start background services
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start service discovery
	go func() {
		if err := serviceDiscovery.Start(ctx); err != nil {
			logrus.Errorf("Service discovery error: %v", err)
		}
	}()

	// Start tenant manager
	go func() {
		if err := tenantManager.Start(ctx); err != nil {
			logrus.Errorf("Tenant manager error: %v", err)
		}
	}()

	// Start remote write controller — always via leader election when enabled so
	// that only one replica writes at a time, preventing duplicate-write artifacts.
	if remoteWriteController != nil && enableRemoteWrite {
		// Resolve leader election namespace: flag > env > default.
		leNS := *leaderElectionNamespace
		if leNS == "" {
			leNS = os.Getenv("POD_NAMESPACE")
		}
		if leNS == "" {
			leNS = "monitoring"
		}

		// Pod identity for the lease holder field.
		podName, _ := os.Hostname()
		if p := os.Getenv("POD_NAME"); p != "" {
			podName = p
		}

		startRemoteWrite := func(leaderCtx context.Context) {
			logrus.Info("Acquired leader election — waiting for service discovery to initialize...")
			time.Sleep(10 * time.Second)
			logrus.Info("Starting remote write controller (leader)")
			if err := remoteWriteController.Start(leaderCtx); err != nil {
				logrus.Errorf("Remote write controller error: %v", err)
			}
		}

		if *leaderElect {
			go func() {
				lock := &resourcelock.LeaseLock{
					LeaseMeta: metav1.ObjectMeta{
						Name:      *leaderElectionID,
						Namespace: leNS,
					},
					Client: k8sClient.CoordinationV1(),
					LockConfig: resourcelock.ResourceLockConfig{
						Identity: podName,
					},
				}
				leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
					Lock:            lock,
					ReleaseOnCancel: true, // release lease on graceful shutdown for fast failover
					LeaseDuration:   15 * time.Second,
					RenewDeadline:   10 * time.Second,
					RetryPeriod:     2 * time.Second,
					Callbacks: leaderelection.LeaderCallbacks{
						OnStartedLeading: func(leaderCtx context.Context) {
							startRemoteWrite(leaderCtx)
						},
						OnStoppedLeading: func() {
							// leaderCtx is already cancelled by the framework;
							// all remote write goroutines stop automatically.
							logrus.Info("Lost leader election — remote write controller stopped")
						},
						OnNewLeader: func(identity string) {
							if identity != podName {
								logrus.Infof("Remote write leader is %s (this pod is standby)", identity)
							}
						},
					},
				})
			}()
		} else {
			// Leader election disabled (single-replica or local dev).
			go startRemoteWrite(ctx)
		}
	} else if remoteWriteController != nil {
		logrus.Info("Remote write controller created but not started (feature disabled in config)")
	}

	// Start server in a goroutine
	go func() {
		logrus.Infof("Server starting on port %d", *port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logrus.Fatalf("Server failed to start: %v", err)
		}
	}()

	// Wait for interrupt signal to gracefully shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logrus.Info("Shutting down server...")

	// Cancel background services
	cancel()

	// Graceful shutdown with timeout
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		logrus.Fatalf("Server forced to shutdown: %v", err)
	}

	logrus.Info("Server exited")
}

// getKubernetesConfig returns the Kubernetes configuration
func getKubernetesConfig(kubeconfigPath string) (*rest.Config, error) {
	if kubeconfigPath != "" {
		// Use provided kubeconfig file
		return clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	}

	// Try in-cluster config first
	if config, err := rest.InClusterConfig(); err == nil {
		return config, nil
	}

	// Fall back to default kubeconfig location
	return clientcmd.BuildConfigFromFlags("", clientcmd.RecommendedHomeFile)
} 