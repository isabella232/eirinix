package extension

import (
	"context"
	"fmt"
	"strconv"
	"time"

	credsgen "code.cloudfoundry.org/cf-operator/pkg/credsgen"
	inmemorycredgen "code.cloudfoundry.org/cf-operator/pkg/credsgen/in_memory_generator"
	"go.uber.org/zap"

	kubeConfig "code.cloudfoundry.org/cf-operator/pkg/kube/config"
	"code.cloudfoundry.org/cf-operator/pkg/kube/util/config"
	"github.com/SUSE/eirinix/util/ctxlog"
	"github.com/pkg/errors"
	"github.com/spf13/afero"
	admissionregistrationv1beta1 "k8s.io/api/admissionregistration/v1beta1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	machinerytypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/runtime/signals"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// DefaultExtensionManager represent an implementation of Manager
type DefaultExtensionManager struct {
	// Extensions is the list of the Extensions that will be registered by the Manager
	Extensions []Extension

	// KubeManager is the kubernetes manager object which is setted up by the Manager
	KubeManager manager.Manager

	// Logger is the logger used internally and accessible to the Extensions
	Logger *zap.SugaredLogger

	// Context is the context structure used by internal components
	Context context.Context

	// WebhookConfig is the webhook configuration used to generate certificates
	WebhookConfig *WebhookConfig

	// WebhookServer is the webhook server where the Manager registers the Extensions to.
	WebhookServer *webhook.Server

	// Credsgen is the credential generator implementation used for generating certificates
	Credsgen credsgen.Generator

	// Options are the manager options
	Options        ManagerOptions
	kubeConnection *rest.Config
}

// ManagerOptions represent the Runtime manager options
type ManagerOptions struct {

	// Namespace is the namespace where the Manager is operating
	Namespace string

	// Host is the listening host address for the Manager
	Host string

	// Port is the listening port
	Port int32

	// KubeConfig is the kubeconfig path. Optional, omit for in-cluster connection
	KubeConfig string

	// Logger is the default logger. Optional, if omitted a new one will be created
	Logger *zap.SugaredLogger

	// FailurePolicy default failure policy for the webhook server.  Optional, defaults to fail
	FailurePolicy *admissionregistrationv1beta1.FailurePolicyType

	// FilterEiriniApps enables or disables Eirini apps filters.  Optional, defaults to true
	FilterEiriniApps *bool

	// OperatorFingerprint is a unique string identifiying the Manager.  Optional, defaults to eirini-x
	OperatorFingerprint string

	// SetupCertificateName is the name of the generated certificates.  Optional, defaults uses OperatorFingerprint to generate a new one
	SetupCertificateName string
}

var addToSchemes = runtime.SchemeBuilder{}

// AddToScheme adds all Resources to the Scheme
func AddToScheme(s *runtime.Scheme) error {
	return addToSchemes.AddToScheme(s)
}

// NewManager returns a manager for the kubernetes cluster.
// the kubeconfig file and the logger are optional
func NewManager(opts ManagerOptions) Manager {

	if opts.Logger == nil {
		z, e := zap.NewProduction()
		if e != nil {
			panic(errors.New("Cannot create logger"))
		}
		defer z.Sync() // flushes buffer, if any
		sugar := z.Sugar()
		opts.Logger = sugar
	}

	if opts.FailurePolicy == nil {
		failurePolicy := admissionregistrationv1beta1.Fail
		opts.FailurePolicy = &failurePolicy
	}

	if len(opts.OperatorFingerprint) == 0 {
		opts.OperatorFingerprint = "eirini-x"
	}

	if len(opts.SetupCertificateName) == 0 {
		opts.SetupCertificateName = opts.getSetupCertificateName()
	}

	if opts.FilterEiriniApps == nil {
		filterEiriniApps := true
		opts.FilterEiriniApps = &filterEiriniApps
	}

	return &DefaultExtensionManager{Options: opts, Logger: opts.Logger}
}

// AddExtension adds an Erini extension to the manager
func (m *DefaultExtensionManager) AddExtension(e Extension) {
	m.Extensions = append(m.Extensions, e)
}

// ListExtensions returns the list of the Extensions added to the Manager
func (m *DefaultExtensionManager) ListExtensions() []Extension {
	return m.Extensions
}

// GetLogger returns the Manager injected logger
func (m *DefaultExtensionManager) GetLogger() *zap.SugaredLogger {
	return m.Logger
}

func (m *DefaultExtensionManager) kubeSetup() error {
	restConfig, err := kubeConfig.NewGetter(m.Logger).Get(m.Options.KubeConfig)
	if err != nil {
		return err
	}
	if err := kubeConfig.NewChecker(m.Logger).Check(restConfig); err != nil {
		return err
	}
	m.kubeConnection = restConfig

	return nil
}

// OperatorSetup prepares the webhook server, generates certificates and configuration.
// It also setups the namespace label for the operator
func (m *DefaultExtensionManager) OperatorSetup() error {
	var err error

	cfg := &config.Config{
		CtxTimeOut:        10 * time.Second,
		Namespace:         m.Options.Namespace,
		WebhookServerHost: m.Options.Host,
		WebhookServerPort: m.Options.Port,
		Fs:                afero.NewOsFs(),
	}

	disableConfigInstaller := true
	m.Context = ctxlog.NewManagerContext(m.Logger)
	m.WebhookConfig = NewWebhookConfig(
		m.KubeManager.GetClient(),
		cfg,
		m.Credsgen,
		fmt.Sprintf("%s-mutating-hook-%s", m.Options.OperatorFingerprint, m.Options.Namespace),
		m.Options.SetupCertificateName)

	hookServer, err := webhook.NewServer(m.Options.OperatorFingerprint, m.KubeManager, webhook.ServerOptions{
		Port:                          m.Options.Port,
		CertDir:                       m.WebhookConfig.CertDir,
		DisableWebhookConfigInstaller: &disableConfigInstaller,
		BootstrapOptions: &webhook.BootstrapOptions{
			MutatingWebhookConfigName: m.WebhookConfig.ConfigName,
			Host:                      &m.Options.Host},
	})
	if err != nil {
		return err
	}
	m.WebhookServer = hookServer

	if err := m.setOperatorNamespaceLabel(); err != nil {
		return errors.Wrap(err, "setting the operator namespace label")
	}

	err = m.WebhookConfig.setupCertificate(m.Context)
	if err != nil {
		return errors.Wrap(err, "setting up the webhook server certificate")
	}
	return nil
}

func (m *DefaultExtensionManager) setOperatorNamespaceLabel() error {
	c := m.KubeManager.GetClient()
	ctx := m.Context
	ns := &unstructured.Unstructured{}
	ns.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "",
		Kind:    "Namespace",
		Version: "v1",
	})
	err := c.Get(ctx, machinerytypes.NamespacedName{Name: m.Options.Namespace}, ns)

	if err != nil {
		return errors.Wrap(err, "getting the namespace object")
	}

	labels := ns.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[m.Options.getDefaultNamespaceLabel()] = m.Options.Namespace
	ns.SetLabels(labels)
	err = c.Update(ctx, ns)

	if err != nil {
		return errors.Wrap(err, "updating the namespace object")
	}

	return nil
}

// GetKubeConnection sets up a connection to a Kubernetes cluster if not existing.
func (m *DefaultExtensionManager) GetKubeConnection() (*rest.Config, error) {
	if m.kubeConnection == nil {
		if err := m.kubeSetup(); err != nil {
			return nil, err
		}
	}
	return m.kubeConnection, nil
}

// RegisterExtensions it generates and register webhooks from the Extensions loaded in the Manager
func (m *DefaultExtensionManager) RegisterExtensions() error {
	webhooks := []*admission.Webhook{}
	for k, e := range m.Extensions {
		w := NewWebhook(e, m)
		admissionHook, err := w.RegisterAdmissionWebHook(
			WebhookOptions{
				ID:             strconv.Itoa(k),
				Manager:        m.KubeManager,
				WebhookServer:  m.WebhookServer,
				ManagerOptions: m.Options,
			})
		if err != nil {
			return err
		}
		webhooks = append(webhooks, admissionHook)
	}

	if err := m.WebhookConfig.generateWebhookServerConfig(m.Context, webhooks); err != nil {
		return errors.Wrap(err, "generating the webhook server configuration")
	}
	return nil
}

func (m *DefaultExtensionManager) setup() error {
	m.Credsgen = inmemorycredgen.NewInMemoryGenerator(m.Logger)
	kubeConn, err := m.GetKubeConnection()
	if err != nil {
		return errors.Wrap(err, "Failed connecting to kubernetes cluster")
	}

	mgr, err := manager.New(
		kubeConn,
		manager.Options{
			Namespace: m.Options.Namespace,
		})
	if err != nil {
		return err
	}

	m.KubeManager = mgr

	if err := m.OperatorSetup(); err != nil {
		return err
	}

	return nil
}

// Start starts the Manager infinite loop, and returns an error on failure
func (m *DefaultExtensionManager) Start() error {
	defer m.Logger.Sync()

	if err := m.setup(); err != nil {
		return err
	}

	// Setup Scheme for all resources
	if err := AddToScheme(m.KubeManager.GetScheme()); err != nil {
		return err
	}

	if err := m.RegisterExtensions(); err != nil {
		return err
	}

	return m.KubeManager.Start(signals.SetupSignalHandler())
}

func (o *ManagerOptions) getDefaultNamespaceLabel() string {
	return fmt.Sprintf("%s-ns", o.OperatorFingerprint)
}

func (o *ManagerOptions) getSetupCertificateName() string {
	return fmt.Sprintf("%s-setupcertificate", o.OperatorFingerprint)
}