package settings

import (
	context "context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/source"

	pom_cfg "github.com/pomerium/pomerium/config"

	icsv1 "github.com/pomerium/ingress-controller/apis/ingress/v1"
	"github.com/pomerium/ingress-controller/controllers/deps"
	"github.com/pomerium/ingress-controller/controllers/reporter"
	"github.com/pomerium/ingress-controller/model"
	"github.com/pomerium/ingress-controller/pomerium"
	"github.com/pomerium/ingress-controller/util"
)

type settingsController struct {
	// key kind/name of a settings object to watch, all others would be ignored
	key model.Key
	// Client is k8s api server client
	client.Client
	// PomeriumReconciler updates Pomerium service configuration
	pomerium.ConfigReconciler
	// Registry is used to keep track of dependencies between objects
	model.Registry
	// MultiPomeriumStatusReporter is used to report when settings are updated
	reporter.MultiPomeriumStatusReporter
	// emitWarnings related to configuration. as there are multiple controllers running, not all should report
	emitWarnings bool
}

// NewSettingsController creates and registers a new controller for
// a given settings object, as we can only watch single settings
func NewSettingsController(
	mgr ctrl.Manager,
	pcr pomerium.ConfigReconciler,
	name types.NamespacedName,
	controllerName string,
	emitWarnings bool,
) error {
	if name.Namespace != "" {
		return fmt.Errorf("pomerium CRD is cluster-scoped")
	}

	key := model.ObjectKey(&icsv1.Pomerium{ObjectMeta: metav1.ObjectMeta{
		Name: name.Name, Namespace: name.Namespace,
	}}, mgr.GetScheme())
	r := model.NewRegistry()

	stc := &settingsController{
		key:              key,
		Client:           deps.NewClient(mgr.GetClient(), r, key),
		Registry:         r,
		ConfigReconciler: pcr,
		MultiPomeriumStatusReporter: []reporter.PomeriumReporter{
			&reporter.SettingsEventReporter{
				EventRecorder: mgr.GetEventRecorderFor(controllerName),
				SettingsReporter: reporter.SettingsReporter{
					NamespacedName: name,
					Client:         mgr.GetClient(),
				},
			},
			&reporter.SettingsStatusReporter{
				SettingsReporter: reporter.SettingsReporter{
					NamespacedName: name,
					Client:         mgr.GetClient(),
				},
			},
			&reporter.SettingsLogReporter{},
		},
		emitWarnings: emitWarnings,
	}

	c, err := ctrl.NewControllerManagedBy(mgr).
		Named(controllerName).
		For(new(icsv1.Pomerium)).
		Build(stc)
	if err != nil {
		return fmt.Errorf("build controller: %w", err)
	}

	for _, o := range []struct {
		client.Object
		mapFn func(model.Registry, string) handler.MapFunc
	}{
		{new(corev1.Secret), deps.GetDependantMapFunc},
	} {
		gvk, err := apiutil.GVKForObject(o.Object, mgr.GetScheme())
		if err != nil {
			return fmt.Errorf("cannot get kind: %w", err)
		}

		err = c.Watch(&source.Kind{Type: o.Object},
			handler.EnqueueRequestsFromMapFunc(o.mapFn(stc.Registry, gvk.Kind)))
		if err != nil {
			return fmt.Errorf("watching %s: %w", gvk.String(), err)
		}
	}
	return nil
}

// Reconcile syncs Settings CRD with pomerium databroker
func (c *settingsController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).V(1)
	if req.NamespacedName != c.key.NamespacedName {
		logger.Info("ignoring", "got", req.NamespacedName, "want", c.key.NamespacedName)
		return ctrl.Result{}, nil
	}

	logger.Info("reconciling... ", "registry", c.Registry)
	c.Registry.DeleteCascade(c.key)

	if c.emitWarnings {
		ctx = util.WithBin[pom_cfg.FieldMsg](ctx)
	}

	cfg, err := FetchConfig(ctx, c.Client, c.key.NamespacedName)
	logger.Info("fetch", "deps", c.Registry.Deps(c.key), "error", err)
	if err != nil {
		c.SettingsRejected(ctx, &cfg.Pomerium, err)
		return ctrl.Result{Requeue: true}, fmt.Errorf("get settings: %w", err)
	}

	if deprecations, err := icsv1.GetDeprecations(&cfg.Pomerium.Spec); err != nil {
		logger.Error(err, "checking config for deprecations")
		util.Add(ctx, pom_cfg.FieldMsg{
			Key:           "pomerium",
			FieldCheckMsg: pom_cfg.FieldCheckMsg(err.Error()),
			KeyAction:     pom_cfg.KeyActionWarn,
		})
	} else {
		util.Add(ctx, deprecations...)
	}

	changed, err := c.SetConfig(ctx, cfg)
	if err != nil {
		if statusUpToDate(&cfg.Pomerium, false) {
			c.SettingsRejected(ctx, &cfg.Pomerium, err)
		}
		return ctrl.Result{Requeue: true}, fmt.Errorf("set config: %w", err)
	}
	if changed || !statusUpToDate(&cfg.Pomerium, true) {
		c.SettingsUpdated(ctx, &cfg.Pomerium)
	}

	return ctrl.Result{}, nil
}

func statusUpToDate(pom *icsv1.Pomerium, reconciled bool) bool {
	if pom.Status.SettingsStatus == nil {
		return false
	}
	if pom.Status.SettingsStatus.Reconciled != reconciled {
		return false
	}
	return pom.Generation == pom.Status.SettingsStatus.ObservedGeneration
}
