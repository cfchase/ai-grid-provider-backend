package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	grpchealth "connectrpc.com/grpchealth/v2"
	grpcreflect "connectrpc.com/grpcreflect/v2"
	"github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1/aigridv1connect"
	"github.com/osac-project/ai-grid-provider-backend/internal/acmsearch"
	"github.com/osac-project/ai-grid-provider-backend/internal/gitops"
	"github.com/osac-project/ai-grid-provider-backend/internal/keycloak"
	"github.com/osac-project/ai-grid-provider-backend/internal/restgateway"
	"github.com/osac-project/ai-grid-provider-backend/internal/servers"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/transport"
)

func main() {
	address := flag.String("listen-address", ":8080", "gRPC listen address")
	httpAddress := flag.String("http-listen-address", ":8081", "REST gateway and Swagger UI listen address")
	kubeconfig := flag.String("kubeconfig", "", "path to kubeconfig; empty uses in-cluster service-account credentials")
	var search searchSettings
	flag.StringVar(&search.url, "search-api-url", acmsearch.DefaultURL, "ACM Search GraphQL endpoint, https only; \"off\" turns cluster GPUs off (a hub without Search, such as Kind)")
	flag.StringVar(&search.caFile, "search-ca-file", "", "CA of ACM Search's serving certificate; empty uses the pod's service CA when present, else the system roots")
	flag.BoolVar(&search.insecureSkipVerify, "search-insecure-skip-verify", false, "skip ACM Search's certificate check; only for a port-forward, so refused unless --search-api-url is a loopback address")
	flag.Parse()

	listener, err := net.Listen("tcp", *address)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen on %s: %v\n", *address, err)
		os.Exit(1)
	}

	restConfig, err := newRestConfig(*kubeconfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load Kubernetes config: %v\n", err)
		os.Exit(1)
	}
	kubeClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create Kubernetes client: %v\n", err)
		os.Exit(1)
	}
	gpuInventory, err := newGPUInventory(restConfig, search)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ACM Search client: %v\n", err)
		os.Exit(1)
	}

	aiGridServer, err := servers.NewAiGridServer().Build()
	if err != nil {
		fmt.Fprintf(os.Stderr, "build AiGrid server: %v\n", err)
		os.Exit(1)
	}
	clustersServer, err := servers.NewClustersServer().
		SetDynamicClient(kubeClient).
		SetReadyPolicy(getEnv("AI_GRID_READY_POLICY", servers.DefaultReadyPolicy)).
		SetGPUInventory(gpuInventory).
		Build()
	if err != nil {
		fmt.Fprintf(os.Stderr, "build Clusters server: %v\n", err)
		os.Exit(1)
	}
	tenantsServer, err := servers.NewTenantsServer().
		SetOrganizationClient(keycloak.New(keycloak.Config{
			BaseURL:    getEnv("KEYCLOAK_URL", "http://keycloak.ai-grid.svc:8080"),
			AdminRealm: getEnv("KEYCLOAK_ADMIN_REALM", "master"),
			Realm:      getEnv("KEYCLOAK_REALM", "ai-grid"),
			Username:   getEnv("KEYCLOAK_ADMIN_USERNAME", "admin"),
			Password:   getEnv("KEYCLOAK_ADMIN_PASSWORD", "admin"),
			// A hung Keycloak call would otherwise hold its tenant's write
			// lock until the caller gives up.
			HTTPClient: &http.Client{Timeout: 30 * time.Second},
		})).
		Build()
	if err != nil {
		fmt.Fprintf(os.Stderr, "build Tenants server: %v\n", err)
		os.Exit(1)
	}
	// Models and Subscriptions are committed to the AI_GRID_GIT_URL
	// repository, and Argo CD delivers them. The status of what came up on
	// the spokes is read back from the catalog's Argo CD Applications on this hub.
	writer, err := newGitWriter()
	if err != nil {
		fmt.Fprintf(os.Stderr, "git writer: %v\n", err)
		os.Exit(1)
	}
	status := servers.NewCatalogStatusReader(kubeClient, servers.CatalogApps{
		Namespace:     getEnv("AI_GRID_CATALOG_APP_NAMESPACE", servers.DefaultCatalogAppNamespace),
		Models:        getEnv("AI_GRID_CATALOG_MODELS_APPSET", servers.DefaultCatalogModelsAppSet),
		Ingress:       getEnv("AI_GRID_CATALOG_INGRESS_APPSET", servers.DefaultCatalogIngressAppSet),
		Subscriptions: getEnv("AI_GRID_CATALOG_SUBSCRIPTIONS_APPSET", servers.DefaultCatalogSubscriptionsAppSet),
	})
	subscriptionNamespace := getEnv("AI_GRID_SUBSCRIPTION_NAMESPACE", "models-as-a-service")
	modelsServer, err := servers.NewModelsServer().
		SetWriter(writer).
		SetCatalogStatus(status).
		SetDynamicClient(kubeClient).
		SetNamespace(getEnv("AI_GRID_MODELS_NAMESPACE", "ai-grid-internal-models")).
		SetSubscriptionNamespace(subscriptionNamespace).
		SetGridNamespace(getEnv("AI_GRID_GRID_NAMESPACE", "redhat-ai-grid")).
		SetReadyPolicy(getEnv("AI_GRID_READY_POLICY", servers.DefaultReadyPolicy)).
		Build()
	if err != nil {
		fmt.Fprintf(os.Stderr, "build Models server: %v\n", err)
		os.Exit(1)
	}
	subscriptionsServer, err := servers.NewSubscriptionsServer().
		SetWriter(writer).
		SetCatalogStatus(status).
		SetNamespace(subscriptionNamespace).
		Build()
	if err != nil {
		fmt.Fprintf(os.Stderr, "build Subscriptions server: %v\n", err)
		os.Exit(1)
	}

	// One *connect.Server holds every service. connecthttp serves Connect,
	// gRPC and gRPC-Web from it; vanguard serves the google.api.http
	// annotations as REST. Both transports dispatch the same handlers.
	server := connect.NewServer()
	aigridv1connect.RegisterAiGridHandler(server, aiGridServer)
	aigridv1connect.RegisterClustersHandler(server, clustersServer)
	aigridv1connect.RegisterTenantsHandler(server, tenantsServer)
	aigridv1connect.RegisterModelsHandler(server, modelsServer)
	aigridv1connect.RegisterSubscriptionsHandler(server, subscriptionsServer)

	grpchealth.Register(server, grpchealth.NewStaticChecker(
		aigridv1connect.AiGridName,
		aigridv1connect.ClustersName,
		aigridv1connect.TenantsName,
		aigridv1connect.ModelsName,
		aigridv1connect.SubscriptionsName,
	))
	// Reflection lists the API services. It does not list grpc.health.v1.Health:
	// grpchealth/v2 registers its methods without a protobuf Schema and grpcreflect
	// skips schema-less methods. Health/Check by name and `grpcurl describe` both work,
	// which is what hack/smoke-apis.sh relies on.
	grpcreflect.Register(server)

	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	if err := restgateway.Mount(mux, server); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	// Plaintext HTTP/2 so gRPC clients (grpcurl, client/, integration/) reach
	// the same address they always have, alongside HTTP/1.1 Connect callers.
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	apiServer := &http.Server{
		Handler:   mux,
		Protocols: protocols,
		// grpc-go applied its own limits to slow and idle connections. A bare
		// net/http server waits on a slow client indefinitely. No WriteTimeout:
		// it would cut long-running streams.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	gateway, err := restgateway.New(*httpAddress, server)
	if err != nil {
		fmt.Fprintf(os.Stderr, "REST gateway: %v\n", err)
		os.Exit(1)
	}

	// http.Server.Shutdown makes Serve return ErrServerClosed immediately, so main
	// must wait for the drain itself: "Make sure the program doesn't exit and waits
	// instead for Shutdown to return." grpc-go's Serve used to block until
	// GracefulStop finished, so without this a pod termination cuts off live RPCs.
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		<-ctx.Done()
		slog.Info("shutting down ai-grid-provider-backend")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// Concurrently: run in sequence and a REST drain that uses the whole
		// deadline leaves the RPC server none, cutting calls still in flight.
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _ = gateway.Shutdown(shutdownCtx) }()
		go func() { defer wg.Done(); _ = apiServer.Shutdown(shutdownCtx) }()
		wg.Wait()
	}()

	go func() {
		if err := gateway.Serve(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "REST gateway: %v\n", err)
			os.Exit(1)
		}
	}()

	slog.Info("ai-grid-provider-backend listening", "grpc", listener.Addr().String(), "http", gateway.Addr())
	if err := apiServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(os.Stderr, "RPC server: %v\n", err)
		os.Exit(1)
	}
	<-drained
}

func getEnv(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

// newGitWriter clones the repository Models and Subscriptions commit to, in
// the background: a Git host that is down, or a wrong token, makes Models and
// Subscriptions answer Unavailable until a retry succeeds, and leaves the other
// APIs serving. Without AI_GRID_GIT_URL they answer FailedPrecondition
// instead; bad settings are still an error at startup. The token is read from
// a file, a mounted Secret, so it never appears in the environment or in logs.
func newGitWriter() (gitops.Writer, error) {
	cfg := gitops.RepoConfig{
		URL:         os.Getenv("AI_GRID_GIT_URL"),
		Branch:      getEnv("AI_GRID_GIT_BRANCH", "main"),
		AuthorName:  "ai-grid-provider-backend",
		AuthorEmail: "ai-grid-provider-backend@noreply.invalid",
	}
	if cfg.URL == "" {
		// A token file without a URL is a half-configured deployment, most
		// likely a Secret missing its url key: stop at startup, as before,
		// rather than refuse Models and Subscriptions later.
		if os.Getenv("AI_GRID_GIT_TOKEN_FILE") != "" {
			return nil, errors.New("AI_GRID_GIT_TOKEN_FILE is set but AI_GRID_GIT_URL is not; set the repository URL, or neither")
		}
		// With no git settings at all, Models and Subscriptions answer
		// FailedPrecondition and the other APIs serve as usual, so a
		// deployment that hasn't set it up yet (the console's chart test, an
		// older install) still serves Clusters and Tenants.
		slog.Warn("AI_GRID_GIT_URL is not set: Models and Subscriptions answer FailedPrecondition until it is")
		return gitops.Unconfigured{}, nil
	}
	if u, err := neturl.Parse(cfg.URL); err != nil || u.User != nil {
		return nil, errors.New("AI_GRID_GIT_URL must be a URL without credentials; put the token in AI_GRID_GIT_TOKEN_FILE")
	}
	if tokenFile := os.Getenv("AI_GRID_GIT_TOKEN_FILE"); tokenFile != "" {
		token, err := os.ReadFile(tokenFile)
		if err != nil {
			return nil, fmt.Errorf("read AI_GRID_GIT_TOKEN_FILE: %w", err)
		}
		cfg.Token = strings.TrimSpace(string(token))
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	slog.Info("git write mode", "url", cfg.URL, "branch", cfg.Branch)
	return gitops.StartBackgroundWriter(context.Background(), func(ctx context.Context) (gitops.Writer, error) {
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		return gitops.NewRepoWriter(ctx, cfg)
	}, time.Second), nil
}

func newRestConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig == "" {
		config, err := rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("load in-cluster Kubernetes config: %w", err)
		}
		return config, nil
	}
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	return config, nil
}

// serviceCAFile is where OpenShift puts the service CA, which signs the
// Search API's serving certificate, in every pod.
const serviceCAFile = "/var/run/secrets/kubernetes.io/serviceaccount/service-ca.crt"

// searchSettings are the ACM Search flags newGPUInventory reads.
type searchSettings struct {
	url                string
	caFile             string
	insecureSkipVerify bool
}

// isLoopback reports whether host is localhost or a loopback IP address.
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// newGPUInventory builds the ACM Search client the Clusters API reads GPUs
// from. It calls Search with the backend's own credential, the one the
// Kubernetes client uses (the ServiceAccount's token in a pod, re-read from
// its file when it rotates; out of one, the kubeconfig's token, token file,
// exec plugin or auth provider, through client-go's own wrappers). Search
// authenticates a bearer token, so a kubeconfig with only a client
// certificate leaves Search calls anonymous: clusters then carry no GPUs,
// and a warning says why.
//
//   - --search-api-url: the GraphQL endpoint (acmsearch.DefaultURL), https
//     only; "off" turns GPUs off, for a hub without ACM Search such as Kind.
//   - --search-ca-file: the CA of Search's serving certificate; the service CA
//     when the file exists, else the system roots.
//   - --search-insecure-skip-verify: no certificate check, for a local run
//     against a port-forwarded Search only, so refused unless
//     --search-api-url is a loopback address: without the check, whoever
//     answers at any other address would receive the credential.
func newGPUInventory(config *rest.Config, s searchSettings) (*servers.GPUInventory, error) {
	url := s.url
	if url == "off" {
		slog.Info("cluster GPUs off: --search-api-url=off")
		return nil, nil
	}
	// The credential goes with every query, so never over plain HTTP.
	u, err := neturl.Parse(url)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("--search-api-url %q must be an https URL, or \"off\"", url)
	}
	if s.insecureSkipVerify && !isLoopback(u.Hostname()) {
		return nil, fmt.Errorf("--search-insecure-skip-verify is only for a port-forward on this machine; --search-api-url %q is not a loopback address", url)
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	caFile := s.caFile
	if caFile == "" {
		if _, err := os.Stat(serviceCAFile); err == nil {
			caFile = serviceCAFile
		}
	}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read --search-ca-file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("--search-ca-file %s holds no PEM certificate", caFile)
		}
		tlsConfig.RootCAs = pool
	}
	if s.insecureSkipVerify {
		slog.Warn("ACM Search certificate not verified: --search-insecure-skip-verify is for local development only")
		tlsConfig.InsecureSkipVerify = true // #nosec G402 -- opt-in for a local port-forward
	}
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.TLSClientConfig = tlsConfig
	// The Kubernetes client's credential wrappers around Search's own TLS:
	// its bearer token or token file, exec plugin or auth provider, and any
	// impersonation, but not its client certificate, which is for the API
	// server's TLS.
	authConfig, err := config.TransportConfig()
	if err != nil {
		return nil, fmt.Errorf("search credential: %w", err)
	}
	if !authConfig.HasTokenAuth() && !authConfig.HasBasicAuth() && authConfig.WrapTransport == nil {
		slog.Warn("no bearer credential for ACM Search: its queries are anonymous, so clusters carry no GPUs; use a kubeconfig with a token, or the in-cluster ServiceAccount")
	}
	rt, err := transport.HTTPWrappersForConfig(authConfig, base)
	if err != nil {
		return nil, fmt.Errorf("search credential: %w", err)
	}
	client := acmsearch.New(url, &http.Client{Transport: rt, Timeout: 15 * time.Second})
	slog.Info("cluster GPUs from ACM Search", "url", url)
	return servers.NewGPUInventory(client, servers.DefaultGPUInventoryTTL, slog.Default()), nil
}
