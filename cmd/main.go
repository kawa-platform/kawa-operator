package main

import (
	"context"
	"crypto/tls"
	"flag"
	"os"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	kawav1alpha1 "github.com/kawa-platform/kawa-operator/api/v1alpha1"
	"github.com/kawa-platform/kawa-operator/internal/controller"
	"github.com/kawa-platform/kawa-operator/internal/kawa"
	// +kubebuilder:scaffold:imports
)

// version is the operator version, set at build time with -ldflags "-X main.version=<version>".
var version = "dev"

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(kawav1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)
	var resyncInterval time.Duration
	var gateway controller.GatewayConfig
	var namespaceSelector, leaderElectionID string
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics server")
	flag.StringVar(&gateway.AdminURL, "gateway-admin-url", "",
		"Base URL of the kawa gateway's admin API, e.g. http://kawa-gateway.kawa.svc:8080 (required).")
	flag.StringVar(&gateway.ClientID, "gateway-client-id", "",
		"The operator's service client id on the gateway (admin.auth.settings.serviceClients); empty without admin auth.")
	flag.StringVar(&gateway.ClientSecretFile, "gateway-client-secret-file", "",
		"File holding the service client's secret, re-read on every use (a mounted Secret).")
	flag.StringVar(&gateway.CAFile, "gateway-ca-file", "", "PEM CA certificates for an https admin API.")
	flag.StringVar(&namespaceSelector, "namespace-selector", "",
		"Label selector of the namespaces whose VirtualTopics, Roles, Groups and Clients are served, "+
			"e.g. kawa-platform.github.io/gateway=kawa; empty serves all.")
	flag.StringVar(&leaderElectionID, "leader-election-id", "kawa-operator.kawa-platform.github.io",
		"Name of the leader election lease; unique per operator installation in a namespace.")
	flag.DurationVar(&resyncInterval, "resync-interval", 5*time.Minute,
		"How often synced resources are compared with the gateway again, undoing changes made outside the operator.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	if err := gateway.Validate(); err != nil {
		setupLog.Error(err, "Invalid gateway configuration")
		os.Exit(1)
	}
	var selector labels.Selector
	if namespaceSelector != "" {
		var err error
		if selector, err = labels.Parse(namespaceSelector); err != nil {
			setupLog.Error(err, "Invalid --namespace-selector")
			os.Exit(1)
		}
	}

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("Disabling HTTP/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.24.1/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.24.1/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// Without a certificate, controller-runtime serves the metrics endpoint with a self-signed one.
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       leaderElectionID,
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	admin := kawa.NewAdmin(gateway.Endpoint)
	logGatewayConnectivity(gateway, admin)
	if err := (&controller.VirtualTopicReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		Admin:             admin,
		NamespaceSelector: selector,
		ResyncInterval:    resyncInterval,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "virtualtopic")
		os.Exit(1)
	}
	if err := (&controller.RoleReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		Admin:             admin,
		NamespaceSelector: selector,
		ResyncInterval:    resyncInterval,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "role")
		os.Exit(1)
	}
	if err := (&controller.GroupReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		Admin:             admin,
		NamespaceSelector: selector,
		ResyncInterval:    resyncInterval,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "group")
		os.Exit(1)
	}
	if err := (&controller.ClientReconciler{
		Client:            mgr.GetClient(),
		APIReader:         mgr.GetAPIReader(),
		Scheme:            mgr.GetScheme(),
		Admin:             admin,
		NamespaceSelector: selector,
		ResyncInterval:    resyncInterval,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "client")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager", "version", version)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}

// logGatewayConnectivity reports at startup whether the operator can reach and authenticate to the
// gateway, acting as itself. It never stops the operator: VirtualTopics report problems as conditions.
func logGatewayConnectivity(gateway controller.GatewayConfig, admin *kawa.Admin) {
	client, err := admin.Client()
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err = client.ListTopics(ctx)
	}
	if err != nil {
		setupLog.Error(err, "Could not reach the gateway admin API yet", "url", gateway.AdminURL)
		return
	}
	setupLog.Info("Connected to the gateway admin API", "url", gateway.AdminURL, "clientId", gateway.ClientID)
}
