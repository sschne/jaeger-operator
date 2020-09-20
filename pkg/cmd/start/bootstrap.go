package start

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"

	osimagev1 "github.com/openshift/api/image/v1"
	"github.com/operator-framework/operator-sdk/pkg/k8sutil"
	kubemetrics "github.com/operator-framework/operator-sdk/pkg/kube-metrics"
	"github.com/operator-framework/operator-sdk/pkg/leader"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"go.opentelemetry.io/otel/api/core"
	"go.opentelemetry.io/otel/api/key"
	"go.opentelemetry.io/otel/exporter/trace/jaeger"
	"go.opentelemetry.io/otel/global"
	"google.golang.org/grpc/codes"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/jaegertracing/jaeger-operator/pkg/apis"
	v1 "github.com/jaegertracing/jaeger-operator/pkg/apis/jaegertracing/v1"
	"github.com/jaegertracing/jaeger-operator/pkg/controller"
	"github.com/jaegertracing/jaeger-operator/pkg/tracing"
	"github.com/jaegertracing/jaeger-operator/pkg/upgrade"
	"github.com/jaegertracing/jaeger-operator/pkg/version"
)

func bootstrap(ctx context.Context) manager.Manager {
	tracing.Bootstrap()

	tracer := global.TraceProvider().GetTracer(v1.BootstrapTracer)
	ctx, span := tracer.Start(ctx, "bootstrap")
	defer span.End()

	setLogLevel(ctx)

	namespace := getNamespace(ctx)

	buildIdentity(ctx, namespace)

	if viper.GetBool("tracing-enabled") {
		buildJaegerExporter(ctx)
	}

	log.WithFields(log.Fields{
		"os":              runtime.GOOS,
		"arch":            runtime.GOARCH,
		"version":         runtime.Version(),
		"operator-sdk":    version.Get().OperatorSdk,
		"jaeger-operator": version.Get().Operator,
		"identity":        viper.GetString(v1.ConfigIdentity),
		"jaeger":          version.Get().Jaeger,
	}).Info("Versions")

	if err := leader.Become(ctx, "jaeger-operator-lock"); err != nil {
		log.Fatal(err)
	}

	cfg, err := config.GetConfig()
	if err != nil {
		span.SetStatus(codes.Internal)
		span.SetAttribute(key.String("error", err.Error()))
		log.Fatal(err)
	}

	span.SetAttribute(key.String("Platform", viper.GetString("platform")))
	watchNamespace, err := k8sutil.GetWatchNamespace()
	if err != nil {
		span.SetStatus(codes.Internal)
		span.SetAttribute(key.String("error", err.Error()))
		log.WithError(err).Fatal("failed to get watch namespace")
	}

	setOperatorScope(ctx, watchNamespace)

	mgr := createManager(ctx, cfg)

	detectNamespacePermissions(ctx, mgr)
	performUpgrades(ctx, mgr)
	setupControllers(ctx, mgr)
	serveCRMetrics(ctx, cfg, namespace)
	detectOAuthProxyImageStream(ctx, mgr)

	return mgr
}

func detectOAuthProxyImageStream(ctx context.Context, mgr manager.Manager) {
	tracer := global.TraceProvider().GetTracer(v1.BootstrapTracer)
	ctx, span := tracer.Start(ctx, "detectOAuthProxyImageStream")
	defer span.End()

	if viper.GetString("platform") != v1.FlagPlatformOpenShift {
		log.Debug("Not running on OpenShift, so won't configure OAuthProxy imagestream.")
		return
	}

	imageStreamNamespace := viper.GetString("openshift-oauth-proxy-imagestream-ns")
	imageStreamName := viper.GetString("openshift-oauth-proxy-imagestream-name")
	if imageStreamNamespace == "" || imageStreamName == "" {
		log.WithFields(log.Fields{
			"namespace": imageStreamNamespace,
			"name":      imageStreamName,
		}).Info("OAuthProxy ImageStream namespace and/or name not defined")
		return
	}

	imageStream := &osimagev1.ImageStream{}
	namespacedName := types.NamespacedName{
		Name:      imageStreamName,
		Namespace: imageStreamNamespace,
	}
	if err := mgr.GetAPIReader().Get(ctx, namespacedName, imageStream); err != nil {
		log.WithFields(log.Fields{
			"namespace": imageStreamNamespace,
			"name":      imageStreamName,
		}).WithError(err).Error("Failed to obtain OAuthProxy ImageStream")
		tracing.HandleError(err, span)
		return
	}

	if len(imageStream.Status.Tags) == 0 {
		log.WithFields(log.Fields{
			"namespace": imageStreamNamespace,
			"name":      imageStreamName,
		}).Error("OAuthProxy ImageStream has no tags")
		return
	}

	if len(imageStream.Status.Tags[0].Items) == 0 {
		log.WithFields(log.Fields{
			"namespace": imageStreamNamespace,
			"name":      imageStreamName,
		}).Error("OAuthProxy ImageStream tag has no items")
		return
	}

	if len(imageStream.Status.Tags[0].Items[0].DockerImageReference) == 0 {
		log.WithFields(log.Fields{
			"namespace": imageStreamNamespace,
			"name":      imageStreamName,
		}).Error("OAuthProxy ImageStream tag has no DockerImageReference")
		return
	}

	image := imageStream.Status.Tags[0].Items[0].DockerImageReference

	viper.Set("openshift-oauth-proxy-image", image)
	log.WithFields(log.Fields{
		"image": image,
	}).Info("Updated OAuth Proxy image flag")
}

func detectNamespacePermissions(ctx context.Context, mgr manager.Manager) {
	tracer := global.TraceProvider().GetTracer(v1.BootstrapTracer)
	ctx, span := tracer.Start(ctx, "detectNamespacePermissions")
	defer span.End()

	namespaces := &corev1.NamespaceList{}
	opts := []client.ListOption{}
	if err := mgr.GetAPIReader().List(ctx, namespaces, opts...); err != nil {
		log.WithError(err).Trace("could not get a list of namespaces, disabling namespace controller")
		tracing.HandleError(err, span)
		span.SetAttribute(key.Bool(v1.ConfigEnableNamespaceController, false))
		viper.Set(v1.ConfigEnableNamespaceController, false)
	} else {
		span.SetAttribute(key.Bool(v1.ConfigEnableNamespaceController, true))
		viper.Set(v1.ConfigEnableNamespaceController, true)
	}
}

func setOperatorScope(ctx context.Context, namespace string) {
	tracer := global.TraceProvider().GetTracer(v1.BootstrapTracer)
	ctx, span := tracer.Start(ctx, "setOperatorScope")
	defer span.End()

	// set what's the namespace to watch
	viper.Set(v1.ConfigWatchNamespace, namespace)

	// for now, the logic is simple: if we are watching all namespaces, then we are cluster-wide
	if viper.GetString(v1.ConfigWatchNamespace) == v1.WatchAllNamespaces {
		span.SetAttribute(key.String(v1.ConfigOperatorScope, v1.OperatorScopeCluster))
		viper.Set(v1.ConfigOperatorScope, v1.OperatorScopeCluster)
	} else {
		log.Info("Consider running the operator in a cluster-wide scope for extra features")
		span.SetAttribute(key.String(v1.ConfigOperatorScope, v1.OperatorScopeNamespace))
		viper.Set(v1.ConfigOperatorScope, v1.OperatorScopeNamespace)
	}
}

func setLogLevel(ctx context.Context) {
	tracer := global.TraceProvider().GetTracer(v1.BootstrapTracer)
	ctx, span := tracer.Start(ctx, "setLogLevel")
	defer span.End()

	level, err := log.ParseLevel(viper.GetString("log-level"))
	if err != nil {
		log.SetLevel(log.InfoLevel)
	} else {
		log.SetLevel(level)
	}
}

func buildIdentity(ctx context.Context, podNamespace string) {
	tracer := global.TraceProvider().GetTracer(v1.BootstrapTracer)
	ctx, span := tracer.Start(ctx, "buildIdentity")
	defer span.End()

	operatorName, found := os.LookupEnv("OPERATOR_NAME")
	if !found {
		log.Warn("the OPERATOR_NAME env var isn't set, this operator's identity might clash with another operator's instance")
	}

	var identity string
	if len(podNamespace) > 0 {
		identity = fmt.Sprintf("%s.%s", podNamespace, operatorName)
	} else {
		identity = fmt.Sprintf("%s", operatorName)
	}

	span.SetAttribute(key.String(v1.ConfigIdentity, identity))
	viper.Set(v1.ConfigIdentity, identity)
}

func buildJaegerExporter(ctx context.Context) {
	tracer := global.TraceProvider().GetTracer(v1.BootstrapTracer)
	ctx, span := tracer.Start(ctx, "buildJaegerExporter")
	defer span.End()

	agentHostPort := viper.GetString("jaeger-agent-hostport")
	jaegerExporter, err := jaeger.NewExporter(
		jaeger.WithAgentEndpoint(agentHostPort),
		jaeger.WithProcess(jaeger.Process{
			ServiceName: "jaeger-operator",
			Tags: []core.KeyValue{
				key.String("operator.identity", viper.GetString(v1.ConfigIdentity)),
				key.String("operator.version", version.Get().Operator),
			},
		}),
		jaeger.WithOnError(func(err error) {
			log.WithError(err).Warn("failed to setup the Jaeger exporter")
		}),
	)
	if err == nil {
		tracing.AddJaegerExporter(jaegerExporter)
	} else {
		span.SetStatus(codes.Internal)
		log.WithError(err).Warn("could not configure a Jaeger tracer for the operator")
	}
}

func createManager(ctx context.Context, cfg *rest.Config) manager.Manager {
	tracer := global.TraceProvider().GetTracer(v1.BootstrapTracer)
	ctx, span := tracer.Start(ctx, "createManager")
	defer span.End()

	namespace := viper.GetString(v1.ConfigWatchNamespace)
	options := manager.Options{
		Namespace:          namespace,
		MetricsBindAddress: fmt.Sprintf("%s:%d", viper.GetString("metrics-host"), viper.GetInt32("metrics-port")),
	}

	// Add support for MultiNamespace set in WATCH_NAMESPACE (e.g ns1,ns2)
	// Note that this is not intended to be used for excluding namespaces, this is better done via a Predicate
	// Also note that you may face performance issues when using this with a high number of namespaces.
	// More Info: https://godoc.org/github.com/kubernetes-sigs/controller-runtime/pkg/cache#MultiNamespacedCacheBuilder
	if strings.Contains(namespace, ",") {
		options.Namespace = ""
		options.NewCache = cache.MultiNamespacedCacheBuilder(strings.Split(namespace, ","))
	}

	// Create a new manager to provide shared dependencies and start components
	mgr, err := manager.New(cfg, options)
	if err != nil {
		span.SetStatus(codes.Internal)
		span.SetAttribute(key.String("error", err.Error()))
		log.Fatal(err)
	}

	// Setup Scheme for all resources
	if err := apis.AddToScheme(mgr.GetScheme()); err != nil {
		span.SetStatus(codes.Internal)
		span.SetAttribute(key.String("error", err.Error()))
		log.Fatal(err)
	}

	return mgr
}

// serveCRMetrics gets the Operator/CustomResource GVKs and generates metrics based on those types.
// It serves those metrics on "http://metricsHost:operatorMetricsPort".
func serveCRMetrics(ctx context.Context, cfg *rest.Config, operatorNs string) {
	tracer := global.TraceProvider().GetTracer(v1.BootstrapTracer)
	ctx, span := tracer.Start(ctx, "serveCRMetrics")
	defer span.End()

	// The function below returns a list of filtered operator/CR specific GVKs. For more control, override the GVK list below
	// with your own custom logic. Note that if you are adding third party API schemas, probably you will need to
	// customize this implementation to avoid permissions issues.
	filteredGVK, err := k8sutil.GetGVKsFromAddToScheme(v1.SchemeBuilder.AddToScheme)
	if err != nil {
		span.SetStatus(codes.Internal)
		log.WithError(err).Warn("could not retrieve group/version/kind managed by this operator")
		return
	}

	// The metrics will be generated from the namespaces which are returned here.
	// NOTE that passing nil or an empty list of namespaces in GenerateAndServeCRMetrics will result in an error.
	ns, err := kubemetrics.GetNamespacesForMetrics(operatorNs)
	if err != nil {
		span.SetStatus(codes.Internal)
		span.SetAttribute(key.String("error", err.Error()))
		log.WithError(err).Warn("could not obtain the namespaces for metrics")
		return
	}

	err = kubemetrics.GenerateAndServeCRMetrics(cfg, ns, filteredGVK, viper.GetString("metrics-host"), viper.GetInt32("cr-metrics-port"))
	if err != nil {
		span.SetStatus(codes.Internal)
		span.SetAttribute(key.String("error", err.Error()))
		log.WithError(err).Warn("could not generate and serve custom resource metrics")
	}
}

func performUpgrades(ctx context.Context, mgr manager.Manager) {
	tracer := global.TraceProvider().GetTracer(v1.BootstrapTracer)
	ctx, span := tracer.Start(ctx, "performUpgrades")
	defer span.End()

	// upgrades all the instances managed by this operator
	if err := upgrade.ManagedInstances(ctx, mgr.GetClient(), mgr.GetAPIReader(), version.Get().Jaeger); err != nil {
		log.WithError(err).Warn("failed to upgrade managed instances")
	}
}

func setupControllers(ctx context.Context, mgr manager.Manager) {
	tracer := global.TraceProvider().GetTracer(v1.BootstrapTracer)
	ctx, span := tracer.Start(ctx, "setupControllers")
	defer span.End()

	if err := controller.AddToManager(mgr); err != nil {
		log.Fatal(err)
	}
}

func getNamespace(ctx context.Context) string {
	tracer := global.TraceProvider().GetTracer(v1.BootstrapTracer)
	ctx, span := tracer.Start(ctx, "getNamespace")
	defer span.End()

	podNamespace, found := os.LookupEnv("POD_NAMESPACE")
	if !found {
		log.Warn("the POD_NAMESPACE env var isn't set, trying to determine it from the service account info")

		var err error
		if podNamespace, err = k8sutil.GetOperatorNamespace(); err != nil {
			span.SetStatus(codes.Internal)
			span.SetAttribute(key.String("error", err.Error()))
			log.WithError(err).Warn("could not read the namespace from the service account")
		}
	}

	return podNamespace
}
